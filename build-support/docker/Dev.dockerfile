FROM hashicorp/consul-k8s:0.24.0
COPY pkg/bin/linux_amd64/consul-k8s /bin
