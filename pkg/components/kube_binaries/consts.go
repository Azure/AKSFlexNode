package kube_binaries

const (
	// Binary installation directory
	binDir = "/usr/local/bin"

	// Kubernetes binaries
	kubeletBinary = "kubelet"
	kubectlBinary = "kubectl"
	kubeadmBinary = "kubeadm"

	// Kubernetes binary paths
	kubeletPath = binDir + "/" + kubeletBinary
	kubectlPath = binDir + "/" + kubectlBinary
	kubeadmPath = binDir + "/" + kubeadmBinary

	// Repository files (these might be used externally, keeping uppercase for now)
	KubernetesRepoList = "/etc/apt/sources.list.d/kubernetes.list"
	KubernetesKeyring  = "/etc/apt/keyrings/kubernetes-apt-keyring.gpg"
)

var (
	kubernetesFileName           = "kubernetes-node-linux-%s.tar.gz"
	defaultKubernetesURLTemplate = "https://acs-mirror.azureedge.net/kubernetes/v%s/binaries/kubernetes-node-linux-%s.tar.gz"
	kubernetesTarPath            = "kubernetes/node/bin/"
)

var kubeBinariesPaths = []string{
	kubeletPath,
	kubectlPath,
	kubeadmPath,
}
