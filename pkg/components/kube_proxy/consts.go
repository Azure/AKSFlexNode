package kube_proxy

const (
	// System directories
	etcKubernetesDir       = "/etc/kubernetes"
	systemdSystemDir       = "/etc/systemd/system"
	kubeProxyServiceDropIn = "/etc/systemd/system/kube-proxy.service.d"

	// Configuration file paths
	kubeProxyConfigPath  = "/var/lib/kube-proxy/config.conf"
	kubeProxyKubeConfig  = "/etc/kubernetes/proxy.conf"
	kubeProxyServicePath = "/etc/systemd/system/kube-proxy.service"
	kubeProxyDropInPath  = "/etc/systemd/system/kube-proxy.service.d/10-cluster-cidr.conf"
	kubeProxyVarDir      = "/var/lib/kube-proxy"

	// Binary paths
	kubeProxyBinaryPath = "/usr/local/bin/kube-proxy"

	// Service configuration
	kubeProxyServiceName = "kube-proxy"

	// File permissions
	configFilePerm = 0644
	binaryFilePerm = 0755
	dirPerm        = 0755

	// Default configuration values
	defaultBindAddress   = "0.0.0.0"
	defaultHealthzPort   = 10256
	defaultMetricsPort   = 10249
	defaultProxyMode     = "iptables"
	defaultClusterCIDR   = "10.244.0.0/16" // Default pod CIDR from CNI
	defaultServiceCIDR   = "10.0.0.0/16"   // Default service CIDR
	defaultConfigMapName = "kube-proxy"
	defaultConfigMapNS   = "kube-system"

	// Kube-proxy download configuration
	kubeProxyDownloadURL = "https://dl.k8s.io/v%s/bin/linux/%s/kube-proxy"
	kubeProxyTempPrefix  = "/tmp/kube-proxy-"
)
