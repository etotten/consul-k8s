package connectinject

import (
	"strings"
	"testing"

	logrtest "github.com/go-logr/logr/testing"
	"github.com/hashicorp/consul/api"
	capi "github.com/hashicorp/consul/api"
	"github.com/hashicorp/consul/sdk/testutil"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TODO: test some error cases for delete service (eg requeue) (not setting up consul agent)
// Todo: test that requeue is true when not IsNotFound error, make sure its not requeued forever

func TestReconcileDeleteEndpoint(t *testing.T) {
	// Create fake k8s client
	// ctx := context.Background()
	client := fake.NewFakeClient()

	// Create test consul server
	consul, err := testutil.NewTestServerConfigT(t, nil)
	require.NoError(t, err)
	defer consul.Stop()

	consul.WaitForLeader(t)
	consulClient, err := capi.NewClient(&capi.Config{
		Address: consul.HTTPAddr,
	})
	require.NoError(t, err)
	addr := strings.Split(consul.HTTPAddr, ":")
	consulPort := addr[1]

	// Register service and proxy in consul
	service := &api.AgentServiceRegistration{
		ID:      "pod1-service-deleted",
		Name:    "service-deleted",
		Port:    80,
		Address: "1.2.3.4",
	}
	err = consulClient.Agent().ServiceRegister(service)
	require.NoError(t, err)
	proxyService := &api.AgentServiceRegistration{
		Kind:    api.ServiceKindConnectProxy,
		ID:      "pod1-service-deleted-sidecar-proxy",
		Name:    "service-deleted-sidecar-proxy",
		Port:    20000,
		Address: "1.2.3.4",
		Proxy: &api.AgentServiceConnectProxyConfig{
			DestinationServiceName: "service-deleted",
			DestinationServiceID:   "pod1-service-deleted",
		},
	}
	err = consulClient.Agent().ServiceRegister(proxyService)
	require.NoError(t, err)

	// Create the endpoints controller
	ep := &EndpointsController{
		Client:       client,
		Log:          logrtest.TestLogger{T: t},
		ConsulClient: consulClient,
		ConsulPort:   consulPort,
		ConsulScheme: "http",
	}
	namespacedName := types.NamespacedName{
		Namespace: "default",
		Name:      "service-deleted",
	}
	resp, err := ep.Reconcile(ctrl.Request{
		NamespacedName: namespacedName,
	})
	require.NoError(t, err)
	require.False(t, resp.Requeue)

	// After reconciliation, Consul should no longer have the service-deleted
	// service and proxyService
	serviceInstances, _, err := consulClient.Catalog().Service("service-deleted", "", nil)
	require.NoError(t, err)
	require.Empty(t, serviceInstances)
}


// First test existing code, then:
// Look at container_init_test where we do service registration to see what we're missing
// i.e tags, processing dc on upstream, prepared queries

//Scenarios
// Add an additonal address in endpoints
// Remove an address from endpoints
// Update an IP in an address
// Test service-name annotation-- delete, add, update

//Test create make sure to have a test that asserts against all fields
// basic service
// service with a port
// service with upstreams
func TestReconcileCreateEndpoint(t *testing.T){
	nodeName := "test-node"

	pod1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod1",
			Namespace: "default",
			Labels: map[string]string{
				labelInject: injected,
			},
			Annotations: map[string]string{
				annotationStatus: injected,
				annotationPort: "1234",
			},
		},
		Status: corev1.PodStatus{
			PodIP:  "1.2.3.4",
			HostIP: "127.0.0.1",
		},
	}
	pod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod2",
			Namespace: "default",
			Labels: map[string]string{
				labelInject: injected,
			},
			Annotations: map[string]string{
				annotationStatus: injected,
				annotationPort: "2234",
			},
		},
		Status: corev1.PodStatus{
			PodIP:  "2.2.3.4",
			HostIP: "127.0.0.1",
		},
	}
	endpointWithTwoAddresses := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "service-created",
			Namespace: "default",
		},
		Subsets: []corev1.EndpointSubset{
			corev1.EndpointSubset{
				Addresses: []corev1.EndpointAddress{
					corev1.EndpointAddress{
						IP:       "1.2.3.4",
						NodeName: &nodeName,
						TargetRef: &corev1.ObjectReference{
							Kind:      "Pod",
							Name:      "pod1",
							Namespace: "default",
						},
					},
					corev1.EndpointAddress{
						IP:       "2.2.3.4",
						NodeName: &nodeName,
						TargetRef: &corev1.ObjectReference{
							Kind:      "Pod",
							Name:      "pod2",
							Namespace: "default",
						},
					},
				},
			},
		},
	}
	client := fake.NewFakeClient(pod1, pod2, endpointWithTwoAddresses)

	// Create test consul server
	consul, err := testutil.NewTestServerConfigT(t, func(c *testutil.TestServerConfig) {
		c.NodeName = nodeName
	})
	require.NoError(t, err)
	defer consul.Stop()

	consul.WaitForLeader(t)
	consulClient, err := capi.NewClient(&capi.Config{
		Address: consul.HTTPAddr,
	})
	require.NoError(t, err)
	addr := strings.Split(consul.HTTPAddr, ":")
	consulPort := addr[1]

	// Create the endpoints controller
	ep := &EndpointsController{
		Client:       client,
		Log:          logrtest.TestLogger{T: t},
		ConsulClient: consulClient,
		ConsulPort:   consulPort,
		ConsulScheme: "http",
	}
	namespacedName := types.NamespacedName{
		Namespace: "default",
		Name:      "service-created",
	}
	resp, err := ep.Reconcile(ctrl.Request{
		NamespacedName: namespacedName,
	})
	require.NoError(t, err)
	require.False(t, resp.Requeue)

	// After reconciliation, Consul should have the service instances
	serviceInstances, _, err := consulClient.Catalog().Service("service-created", "", nil)
	require.NoError(t, err)
	require.Len(t, serviceInstances, 2)
	require.Equal(t, service.ID, serviceInstances[0].ServiceID)
	require.Equal(t, service.Address, serviceInstances[0].ServiceAddress)
	require.Equal(t, "pod2-service-updated", serviceInstances[1].ServiceID)
	require.Equal(t, "2.2.3.4", serviceInstances[1].ServiceAddress)
	proxyServiceInstances, _, err := consulClient.Catalog().Service("service-created-sidecar-proxy", "", nil)
	require.NoError(t, err)
	require.Len(t, proxyServiceInstances, 2)
	require.Equal(t, proxyService.ID, proxyServiceInstances[0].ServiceID)
	require.Equal(t, proxyService.Address, proxyServiceInstances[0].ServiceAddress)
	require.Equal(t, "pod2-service-updated-sidecar-proxy", proxyServiceInstances[1].ServiceID)
	require.Equal(t, "2.2.3.4", proxyServiceInstances[1].ServiceAddress)
}

// Adds additional addresses in endpoints to consul
func TestReconcileUpdateEndpoint(t *testing.T) {
	// Create fake k8s client
	// ctx := context.Background()

	nodeName := "test-node"

	pod1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod1",
			Namespace: "default",
			Labels: map[string]string{
				labelInject: injected,
			},
			Annotations: map[string]string{
				annotationStatus: injected,
			},
		},
		Status: corev1.PodStatus{
			PodIP:  "1.2.3.4",
			HostIP: "127.0.0.1",
		},
	}
	pod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod2",
			Namespace: "default",
			Labels: map[string]string{
				labelInject: injected,
			},
			Annotations: map[string]string{
				annotationStatus: injected,
			},
		},
		Status: corev1.PodStatus{
			PodIP:  "2.2.3.4",
			HostIP: "127.0.0.1",
		},
	}
	endpointWithTwoAddresses := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "service-updated",
			Namespace: "default",
		},
		Subsets: []corev1.EndpointSubset{
			corev1.EndpointSubset{
				Addresses: []corev1.EndpointAddress{
					corev1.EndpointAddress{
						IP:       "1.2.3.4",
						NodeName: &nodeName,
						TargetRef: &corev1.ObjectReference{
							Kind:      "Pod",
							Name:      "pod1",
							Namespace: "default",
						},
					},
					corev1.EndpointAddress{
						IP:       "2.2.3.4",
						NodeName: &nodeName,
						TargetRef: &corev1.ObjectReference{
							Kind:      "Pod",
							Name:      "pod2",
							Namespace: "default",
						},
					},
				},
			},
		},
	}
	client := fake.NewFakeClient(pod1, pod2, endpointWithTwoAddresses)

	// Create test consul server
	consul, err := testutil.NewTestServerConfigT(t, func(c *testutil.TestServerConfig) {
		c.NodeName = nodeName
	})
	require.NoError(t, err)
	defer consul.Stop()

	consul.WaitForLeader(t)
	consulClient, err := capi.NewClient(&capi.Config{
		Address: consul.HTTPAddr,
	})
	require.NoError(t, err)
	addr := strings.Split(consul.HTTPAddr, ":")
	consulPort := addr[1]

	// Register service and proxy in consul
	service := &api.AgentServiceRegistration{
		ID:      "pod1-service-updated",
		Name:    "service-updated",
		Port:    80,
		Address: "1.2.3.4",
	}
	err = consulClient.Agent().ServiceRegister(service)
	require.NoError(t, err)
	proxyService := &api.AgentServiceRegistration{
		Kind:    api.ServiceKindConnectProxy,
		ID:      "pod1-service-updated-sidecar-proxy",
		Name:    "service-updated-sidecar-proxy",
		Port:    20000,
		Address: "1.2.3.4",
		Proxy: &api.AgentServiceConnectProxyConfig{
			DestinationServiceName: "service-updated",
			DestinationServiceID:   "pod1-service-updated",
		},
	}
	err = consulClient.Agent().ServiceRegister(proxyService)
	require.NoError(t, err)

	// Create the endpoints controller
	ep := &EndpointsController{
		Client:       client,
		Log:          logrtest.TestLogger{T: t},
		ConsulClient: consulClient,
		ConsulPort:   consulPort,
		ConsulScheme: "http",
	}
	namespacedName := types.NamespacedName{
		Namespace: "default",
		Name:      "service-updated",
	}
	resp, err := ep.Reconcile(ctrl.Request{
		NamespacedName: namespacedName,
	})
	require.NoError(t, err)
	require.False(t, resp.Requeue)

	// After reconciliation, Consul should have service-updated with 2 instances
	serviceInstances, _, err := consulClient.Catalog().Service("service-updated", "", nil)
	require.NoError(t, err)
	require.Len(t, serviceInstances, 2)
	require.Equal(t, service.ID, serviceInstances[0].ServiceID)
	require.Equal(t, service.Address, serviceInstances[0].ServiceAddress)
	require.Equal(t, "pod2-service-updated", serviceInstances[1].ServiceID)
	require.Equal(t, "2.2.3.4", serviceInstances[1].ServiceAddress)
	proxyServiceInstances, _, err := consulClient.Catalog().Service("service-updated-sidecar-proxy", "", nil)
	require.NoError(t, err)
	require.Len(t, proxyServiceInstances, 2)
	require.Equal(t, proxyService.ID, proxyServiceInstances[0].ServiceID)
	require.Equal(t, proxyService.Address, proxyServiceInstances[0].ServiceAddress)
	require.Equal(t, "pod2-service-updated-sidecar-proxy", proxyServiceInstances[1].ServiceID)
	require.Equal(t, "2.2.3.4", proxyServiceInstances[1].ServiceAddress)
}
