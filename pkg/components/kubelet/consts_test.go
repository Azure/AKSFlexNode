package kubelet

import (
	"testing"
)

// TestKubeletConstants verifies kubelet configuration path constants.
// Test: Validates all kubelet-related paths including config, service files, and data directories
// Expected: All paths should match standard Kubernetes kubelet installation locations
func TestKubeletConstants(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		expected string
	}{
		{"etcDefaultDir", etcDefaultDir, "/etc/default"},
		{"kubeletServiceDir", kubeletServiceDir, "/etc/systemd/system/kubelet.service.d"},
		{"etcKubernetesDir", etcKubernetesDir, "/etc/kubernetes"},
		{"kubeletManifestsDir", kubeletManifestsDir, "/etc/kubernetes/manifests"},
		{"kubeletVolumePluginDir", kubeletVolumePluginDir, "/etc/kubernetes/volumeplugins"},
		{"kubeletDefaultsPath", kubeletDefaultsPath, "/etc/default/kubelet"},
		{"kubeletServicePath", kubeletServicePath, "/etc/systemd/system/kubelet.service"},
		{"kubeletContainerdConfig", kubeletContainerdConfig, "/etc/systemd/system/kubelet.service.d/10-containerd.conf"},
		{"kubeletTLSBootstrapConfig", kubeletTLSBootstrapConfig, "/etc/systemd/system/kubelet.service.d/10-tlsbootstrap.conf"},
		{"kubeletConfigPath", kubeletConfigPath, "/var/lib/kubelet/config.yaml"},
		{"kubeletKubeConfig", kubeletKubeConfig, "/etc/kubernetes/kubelet.conf"},
		{"kubeletBootstrapKubeConfig", kubeletBootstrapKubeConfig, "/etc/kubernetes/bootstrap-kubelet.conf"},
		{"kubeletVarDir", kubeletVarDir, "/var/lib/kubelet"},
		{"kubeletKubeconfigPath", kubeletKubeconfigPath, "/var/lib/kubelet/kubeconfig"},
		{"kubeletTokenScriptPath", kubeletTokenScriptPath, "/var/lib/kubelet/token.sh"},
		{"aksServiceResourceID", aksServiceResourceID, "6dae42f8-4368-4678-94ff-3960e28e3630"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.value != tt.expected {
				t.Errorf("%s = %s, want %s", tt.name, tt.value, tt.expected)
			}
		})
	}
}
