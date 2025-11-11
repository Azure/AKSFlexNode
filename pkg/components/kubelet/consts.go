package kubelet

const (
	// System directories
	EtcDefaultDir       = "/etc/default"
	KubeletServiceDir   = "/etc/systemd/system/kubelet.service.d"

	// Configuration file paths
	KubeletDefaultsPath      = "/etc/default/kubelet"
	KubeletServicePath       = "/etc/systemd/system/kubelet.service"
	KubeletContainerdConfig  = "/etc/systemd/system/kubelet.service.d/10-containerd.conf"
	KubeletTLSBootstrapConfig = "/etc/systemd/system/kubelet.service.d/10-tlsbootstrap.conf"

	// Runtime configuration paths
	KubeletConfigPath          = "/var/lib/kubelet/config.yaml"
	KubeletKubeConfig          = "/etc/kubernetes/kubelet.conf"
	KubeletBootstrapKubeConfig = "/etc/kubernetes/bootstrap-kubelet.conf"
	KubeletVarDir              = "/var/lib/kubelet"
	KubeletKubeconfigPath      = "/var/lib/kubelet/kubeconfig"
	KubeletTokenScriptPath     = "/var/lib/kubelet/token.sh"

	// Azure resource identifiers
	AKSServiceResourceID       = "6dae42f8-4368-4678-94ff-3960e28e3630"
)