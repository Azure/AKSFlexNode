package config

const (
	DefaultContainerdMetricsAddress = "0.0.0.0:10257"
	DefaultSandboxImage             = "mcr.microsoft.com/oss/kubernetes/pause:3.9"

	DefaultCNIBinDir    = "/opt/cni/bin"
	DefaultCNIConfigDir = "/etc/cni/net.d"

	DefaultBinaryPath = "/usr/local/bin"

	DefaultCNIPluginsVersion = "1.5.1"
	DefaultCNISpecVersion    = "0.3.1"
	DefaultNPDVersion        = "v1.35.1"
	DefaultRunCVersion       = "1.1.12"
	DefaultContainerdVersion = "2.0.4" // FIXME: confirm if we still want containerd 1.x

	KubeletKubeconfigPath = "/var/lib/kubelet/kubeconfig"
)
