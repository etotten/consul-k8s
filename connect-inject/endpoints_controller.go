package connectinject

import (
	"context"
	"fmt"
	"strings"

	mapset "github.com/deckarep/golang-set"
	"github.com/go-logr/logr"
	"github.com/hashicorp/consul/api"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// todo: add docs
type EndpointsController struct {
	client.Client
	// ConsulClient points at the agent local to the connect-inject deployment pod
	ConsulClient *api.Client
	// ConsulScheme is the scheme to use when making API calls to Consul,
	// i.e. "http" or "https".
	ConsulScheme string
	// ConsulPort is the port to make HTTP API calls to Consul agents on.
	ConsulPort            string
	AllowK8sNamespacesSet mapset.Set
	DenyK8sNamespacesSet  mapset.Set
	Log                   logr.Logger
	Scheme                *runtime.Scheme
}

// TODOs:
// 1. in some error cases, we need to requeue in the result rather than returning an empty result
func (r *EndpointsController) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	var serviceEndpoints corev1.Endpoints

	// Ignore namespaces where we don't connect-inject.
	// Ignores system namespaces.
	if req.Namespace == "kube-system" || req.Namespace == "local-path-storage" {
		return ctrl.Result{}, nil
	}
	// Ignores deny list.
	if r.DenyK8sNamespacesSet.Contains(req.Namespace) {
		return ctrl.Result{}, nil
	}
	// Ignores if not in allow list or allow list is not *.
	if r.AllowK8sNamespacesSet.Contains("*") && !r.AllowK8sNamespacesSet.Contains(req.Namespace) {
		return ctrl.Result{}, nil
	}

	proxyServiceName := fmt.Sprintf("%s-sidecar-proxy", req.Name)
	err := r.Client.Get(context.Background(), req.NamespacedName, &serviceEndpoints)

	// If the endpoints object has been deleted, we need to deregister all instances for that service.
	if k8serrors.IsNotFound(err) {
		for _, name := range []string{req.Name, proxyServiceName} {
			// ? Q: annotationService, vs 1st container name on pod vs actual K8s svc name??
			// todo: handle a case when the name has been overwritten by the annotation
			// in delete, we would get the k8s svc name from metadata on svc instance
			// in create/update, we would get it from the pod annotation
			// Always query based on metadata rather than name
			// To filter based on name, we need to either use the Agent endpoint on every agent or query all services and then use service with the query meta
			serviceInstances, _, err := r.ConsulClient.Catalog().Service(name, "", nil)
			if err != nil {
				r.Log.Error(err, "failed to get service instances from Consul", "name", name)
				return ctrl.Result{}, err
			}
			for _, instance := range serviceInstances {
				// ? Q: why do we need this? why not use r.ConsulClient? wouldn't this be pod ip of the svc instance??
				agentClient, err := r.getConsulClient(instance.Address) // this is the pod IP of the consul client agent rather than service address
				if err != nil {
					r.Log.Error(err, "failed to create a new Consul client", "address", instance.Address)
					return ctrl.Result{}, err
				}
				// ? Q: are we deregistering the service multiple times? per instance rather than per svc?
				r.Log.Info("deregistering service", "service", instance.ServiceName)
				err = agentClient.Agent().ServiceDeregister(instance.ServiceID)
				if err != nil {
					r.Log.Error(err, "failed to deregister service", "name", name)
					return ctrl.Result{}, err
				}
			}

			return ctrl.Result{}, nil
		}
	} else if err != nil {
		r.Log.Error(err, "failed to get endpoints from Kubernetes", "namespace", req.Namespace, "name", req.Name)
		return ctrl.Result{}, err
	}

	r.Log.Info("retrieved service from kube", "serviceEndpoints", serviceEndpoints)

	// Register all addresses with Consul
	for _, subset := range serviceEndpoints.Subsets {
		// Do the same thing for all addresses, regardless of whether they're ready
		allAddresses := subset.Addresses
		allAddresses = append(allAddresses, subset.NotReadyAddresses...)

		r.Log.Info("all addresses", "addresses", allAddresses)
		for _, address := range allAddresses {
			if address.TargetRef != nil && address.TargetRef.Kind == "Pod" {
				var pod corev1.Pod
				objectKey := types.NamespacedName{Name: address.TargetRef.Name, Namespace: address.TargetRef.Namespace}
				err = r.Client.Get(context.Background(), objectKey, &pod)
				if err != nil {
					r.Log.Error(err, "failed to get pod from Kubernetes", "pod name", address.TargetRef.Name)
					return ctrl.Result{}, err
				}

				// ? Q: will this be set in time
				// "has it been injected"
				if r.willBeInjected(&pod) {
					// get consul client
					client, err := r.getConsulClient(pod.Status.HostIP)
					if err != nil {
						r.Log.Error(err, "failed to create a new Consul client", "address", pod.Status.HostIP)
						return ctrl.Result{}, err
					}
					// If a port is specified, then we determine the value of that port
					// and register that port for the host service.
					var servicePort int
					if raw, ok := pod.Annotations[annotationPort]; ok && raw != "" {
						if port, _ := portValue(&pod, raw); port > 0 {
							servicePort = int(port)
						}
					}
					serviceID := fmt.Sprintf("%s-%s", pod.Name, serviceEndpoints.Name)
					service := &api.AgentServiceRegistration{
						ID:        serviceID,
						Name:      serviceEndpoints.Name, // todo handle annotation
						Tags:      nil,                   // todo: process tags from annotations
						Port:      servicePort,
						Address:   pod.Status.PodIP,
						Meta:      map[string]string{"pod-name": pod.Name}, // todo process user-provided meta tag; it's missing the latest metadata for k8s-service-name, k8s-namespace
						Namespace: "",                                      // todo deal with namespaces
					}
					r.Log.Info("registering service", "service", service)
					err = client.Agent().ServiceRegister(service)
					if err != nil {
						r.Log.Error(err, "failed to register service with Consul", "service name", service.Name)
						return ctrl.Result{}, err
					}

					proxyServiceID := fmt.Sprintf("%s-%s", pod.Name, proxyServiceName)
					proxyConfig := &api.AgentServiceConnectProxyConfig{
						DestinationServiceName: serviceEndpoints.Name,
						DestinationServiceID:   serviceID,
						Config:                 nil, // todo: add config for metrics
					}

					if servicePort > 0 {
						proxyConfig.LocalServiceAddress = "127.0.0.1"
						proxyConfig.LocalServicePort = servicePort
					}

					proxyConfig.Upstreams = processUpstreams(&pod)

					proxyService := &api.AgentServiceRegistration{
						Kind:            api.ServiceKindConnectProxy,
						ID:              proxyServiceID,
						Name:            proxyServiceName,
						Tags:            nil, // todo: same as service tags
						Port:            20000,
						Address:         pod.Status.PodIP,
						TaggedAddresses: nil,                                     // todo: set cluster IP here (will be done later)
						Meta:            map[string]string{"pod-name": pod.Name}, // todo: same as service meta; also add k8s-namespace meta
						Namespace:       "",                                      // todo: same as service namespace
						Proxy:           proxyConfig,
						Check:           nil,
						Checks: api.AgentServiceChecks{
							{
								Name:                           "Proxy Public Listener",
								TCP:                            fmt.Sprintf("%s:20000", pod.Status.PodIP),
								Interval:                       "10s",
								DeregisterCriticalServiceAfter: "10m",
							},
							{
								Name:         "Destination Alias",
								AliasService: serviceID,
							},
						},
						Connect: nil,
					}

					r.Log.Info("registering proxy service", "service", proxyService)
					err = client.Agent().ServiceRegister(proxyService)
					if err != nil {
						r.Log.Error(err, "failed to register proxy service with Consul", "service name", proxyServiceName)
						return ctrl.Result{}, err
					}
				}
			}
		}
	}

	// todo: we'd also need to reconcile existing service instances in consul to make sure they have pods in k8s
	// if they don't we should deregister
	// 1. getting all service instances for this service from consul (see cleanup_resource.go line 124)
	// 2. compare allAddresses with services instances
	// 3. loop through all service instances
	//      if a service instance is not in allAddresses, deregister it

	return ctrl.Result{}, nil
}

func processUpstreams(pod *corev1.Pod) []api.Upstream {
	var upstreams []api.Upstream
	if raw, ok := pod.Annotations[annotationUpstreams]; ok && raw != "" {
		for _, raw := range strings.Split(raw, ",") {
			parts := strings.SplitN(raw, ":", 3)

			var datacenter, serviceName, preparedQuery string
			var port int32
			if strings.TrimSpace(parts[0]) == "prepared_query" {
				port, _ = portValue(pod, strings.TrimSpace(parts[2]))
				preparedQuery = strings.TrimSpace(parts[1])
			} else {
				port, _ = portValue(pod, strings.TrimSpace(parts[1]))

				// todo: Parse the namespace if provided
				//if data.ConsulNamespace != "" {
				//	pieces := strings.SplitN(parts[0], ".", 2)
				//	serviceName = pieces[0]
				//
				//	if len(pieces) > 1 {
				//		namespace = pieces[1]
				//	}
				//} else {
				//	serviceName = strings.TrimSpace(parts[0])
				//}

				serviceName = strings.TrimSpace(parts[0])

				// parse the optional datacenter
				if len(parts) > 2 {
					datacenter = strings.TrimSpace(parts[2])
				}
			}

			if port > 0 {
				upstream := api.Upstream{
					DestinationType:      api.UpstreamDestTypeService,
					DestinationNamespace: "", // todo
					DestinationName:      serviceName,
					Datacenter:           datacenter,
					LocalBindPort:        int(port),
				}

				if preparedQuery != "" {
					upstream.DestinationType = api.UpstreamDestTypePreparedQuery
					upstream.DestinationName = preparedQuery
				}

				upstreams = append(upstreams, upstream)
			}
		}
	}

	return upstreams
}

func (r *EndpointsController) willBeInjected(pod *corev1.Pod) bool {
	// todo: make sure this doesn't panic if a pod has not been injected
	// because then this annotation will not be present
	if pod.Annotations[annotationStatus] != injected {
		return false
	}

	return true
}

// getConsulClient returns an *api.Client that points at the consul agent local to the pod.
func (r *EndpointsController) getConsulClient(ip string) (*api.Client, error) {
	// todo: un-hardcode the scheme and port
	newAddr := fmt.Sprintf("%s://%s:%s", r.ConsulScheme, ip, r.ConsulPort)
	localConfig := api.DefaultConfig()
	localConfig.Address = newAddr

	localClient, err := api.NewClient(localConfig)
	if err != nil {
		return nil, err
	}

	return localClient, err
}

func (r *EndpointsController) Logger(name types.NamespacedName) logr.Logger {
	return r.Log.WithValues("request", name)
}

func (r *EndpointsController) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Endpoints{}).
		Complete(r)
}
