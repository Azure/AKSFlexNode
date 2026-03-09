package config

const (
	DefaultContainerdMetricsAddress = "0.0.0.0:10257"
	DefaultSandboxImage             = "mcr.microsoft.com/oss/kubernetes/pause:3.9"

	DefaultCNIBinDir    = "/opt/cni/bin"
	DefaultCNIConfigDir = "/etc/cni/net.d"

	DefaultBinaryPath = "/usr/local/bin"

	DefaultNvidiaContainerRuntimePath = "/usr/bin/nvidia-container-runtime"
	DefaultNvidiaRuntimeClassName     = "nvidia"

	DefaultCNIPluginsVersion = "1.5.1"
	DefaultCNISpecVersion    = "0.3.1"
	DefaultNPDVersion        = "v1.35.1"
	DefaultRunCVersion       = "1.1.12"
	DefaultContainerdVersion = "2.0.4" // FIXME: confirm if we still want containerd 1.x

)

// refs:
// - https://kubernetes.io/docs/reference/node/kubelet-files/
// - https://kubernetes.io/docs/reference/setup-tools/kubeadm/kubeadm-reset/
const (
	SystemdUnitKubelet = "kubelet.service"

	KubeletRoot                    = "/var/lib/kubelet"
	KubeletKubeconfigPath          = KubeletRoot + "/kubelet/kubeconfig"
	KubeletBootstrapKubeconfigPath = KubeletRoot + "/bootstrap-kubeconfig"
	KubeletStaticPodPath           = "/etc/kubernetes/manifests"

	KubernetesConfigDir = "/etc/kubernetes"
	KubernetesRunDir    = "/var/run/kubernetes"
	CNIStateDir         = "/var/lib/cni"
)
