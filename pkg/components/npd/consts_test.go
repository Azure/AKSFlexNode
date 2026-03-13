package npd

import (
	"testing"
)

// TestNPDConstants verifies Node Problem Detector (NPD) path constants.
// Test: Validates NPD binary, config, service paths, and temp directory
// Expected: All paths should match standard NPD installation locations
func TestNPDConstants(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		expected string
	}{
		{"npdBinaryPath", npdBinaryPath, "/usr/bin/node-problem-detector"},
		{"npdConfigPath", npdConfigPath, "/etc/node-problem-detector/kernel-monitor.json"},
		{"npdServicePath", npdServicePath, "/etc/systemd/system/node-problem-detector.service"},
		{"kubeletKubeconfigPath", kubeletKubeconfigPath, "/var/lib/kubelet/kubeconfig"},
		{"tempDir", tempDir, "/tmp/npd"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.value != tt.expected {
				t.Errorf("%s = %s, want %s", tt.name, tt.value, tt.expected)
			}
		})
	}
}

// TestNPDVariables verifies NPD download configuration variables.
// Test: Validates filename template and download URL template
// Expected: Variables should contain proper format specifiers for version and architecture
func TestNPDVariables(t *testing.T) {
	if npdFileName == "" {
		t.Error("npdFileName should not be empty")
	}

	if npdDownloadURL == "" {
		t.Error("npdDownloadURL should not be empty")
	}

	expectedFileName := "npd-%s.tar.gz"
	if npdFileName != expectedFileName {
		t.Errorf("npdFileName = %s, want %s", npdFileName, expectedFileName)
	}
}
