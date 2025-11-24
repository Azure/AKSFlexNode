package kubernetes_components

const (
	// Binary installation directory
	BinDir = "/usr/local/bin"

	// Kubernetes binaries
	KubeletBinary = "kubelet"
	KubectlBinary = "kubectl"
	KubeadmBinary = "kubeadm"

	// Kubernetes binary paths
	KubeletPath = BinDir + "/" + KubeletBinary
	KubectlPath = BinDir + "/" + KubectlBinary
	KubeadmPath = BinDir + "/" + KubeadmBinary

	// Repository files
	KubernetesRepoList = "/etc/apt/sources.list.d/kubernetes.list"
	KubernetesKeyring  = "/etc/apt/keyrings/kubernetes-apt-keyring.gpg"
)
