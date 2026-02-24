package config

const (
	DefaultContainerdMetricsAddress = "0.0.0.0:10257"
	DefaultSandboxImage             = "mcr.microsoft.com/oss/kubernetes/pause:3.9"

	DefaultCNIBinDir    = "/opt/cni/bin"
	DefaultCNIConfigDir = "/etc/cni/net.d"

	DefaultBinaryPath = "/usr/local/bin"

	DefaultNPDVersion = "v1.35.1"
)
