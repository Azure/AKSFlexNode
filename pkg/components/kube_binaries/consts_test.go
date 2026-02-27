package kube_binaries

import (
	"testing"
)

// TestKubeBinariesConstants verifies all Kubernetes binary path constants.
// Test: Validates binary directory, binary names, and full paths for kubectl, kubelet, kubeadm
// Expected: All paths should match standard Kubernetes installation locations (/usr/local/bin)
func TestKubeBinariesConstants(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		expected string
	}{
		{"binDir", binDir, "/usr/local/bin"},
		{"kubeletBinary", kubeletBinary, "kubelet"},
		{"kubectlBinary", kubectlBinary, "kubectl"},
		{"kubeadmBinary", kubeadmBinary, "kubeadm"},
		{"kubeletPath", kubeletPath, "/usr/local/bin/kubelet"},
		{"kubectlPath", kubectlPath, "/usr/local/bin/kubectl"},
		{"kubeadmPath", kubeadmPath, "/usr/local/bin/kubeadm"},
		{"KubernetesRepoList", KubernetesRepoList, "/etc/apt/sources.list.d/kubernetes.list"},
		{"KubernetesKeyring", KubernetesKeyring, "/etc/apt/keyrings/kubernetes-apt-keyring.gpg"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.value != tt.expected {
				t.Errorf("%s = %s, want %s", tt.name, tt.value, tt.expected)
			}
		})
	}
}

// TestKubeBinariesVariables verifies Kubernetes download configuration variables and binary paths array.
// Test: Validates filename template, URL template, tar path, and binary paths array
// Expected: Variables should have proper format specifiers, array should contain all 3 binary paths
func TestKubeBinariesVariables(t *testing.T) {
	if kubernetesFileName == "" {
		t.Error("kubernetesFileName should not be empty")
	}

	if defaultKubernetesURLTemplate == "" {
		t.Error("defaultKubernetesURLTemplate should not be empty")
	}

	if kubernetesTarPath == "" {
		t.Error("kubernetesTarPath should not be empty")
	}

	// Test kubeBinariesPaths array
	if len(kubeBinariesPaths) != 3 {
		t.Errorf("Expected 3 binary paths, got %d", len(kubeBinariesPaths))
	}

	expectedPaths := []string{
		"/usr/local/bin/kubelet",
		"/usr/local/bin/kubectl",
		"/usr/local/bin/kubeadm",
	}

	for i, path := range kubeBinariesPaths {
		if path != expectedPaths[i] {
			t.Errorf("kubeBinariesPaths[%d] = %s, want %s", i, path, expectedPaths[i])
		}
	}
}
