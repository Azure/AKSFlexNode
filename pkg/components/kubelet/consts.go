package kubelet

const (
	// System directories
	etcDefaultDir     = "/etc/default"
	kubeletServiceDir = "/etc/systemd/system/kubelet.service.d"
	etcKubernetesDir  = "/etc/kubernetes"

	// Kubelet-specific directories
	kubeletManifestsDir    = "/etc/kubernetes/manifests"
	kubeletVolumePluginDir = "/etc/kubernetes/volumeplugins"

	// Configuration file paths
	kubeletDefaultsPath       = "/etc/default/kubelet"
	kubeletServicePath        = "/etc/systemd/system/kubelet.service"
	kubeletContainerdConfig   = "/etc/systemd/system/kubelet.service.d/10-containerd.conf"
	kubeletTLSBootstrapConfig = "/etc/systemd/system/kubelet.service.d/10-tlsbootstrap.conf"

	// Runtime configuration paths
	kubeletConfigPath          = "/var/lib/kubelet/config.yaml"
	kubeletKubeConfig          = "/etc/kubernetes/kubelet.conf"
	kubeletBootstrapKubeConfig = "/etc/kubernetes/bootstrap-kubelet.conf"
	kubeletVarDir              = "/var/lib/kubelet"
	KubeletKubeconfigPath      = "/var/lib/kubelet/kubeconfig"
	kubeletTokenScriptPath     = "/var/lib/kubelet/token.sh"

	// Azure resource identifiers
	aksServiceResourceID = "6dae42f8-4368-4678-94ff-3960e28e3630"
)
