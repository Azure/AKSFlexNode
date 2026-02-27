package containerd

import (
	"testing"
)

// TestContainerdConstants verifies containerd installation path constants.
// Test: Validates binary directory, config directory, service file, and data directory paths
// Expected: All paths should match standard containerd installation locations
func TestContainerdConstants(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		expected string
	}{
		{"systemBinDir", systemBinDir, "/usr/bin"},
		{"defaultContainerdBinaryDir", defaultContainerdBinaryDir, "/usr/bin/containerd"},
		{"defaultContainerdConfigDir", defaultContainerdConfigDir, "/etc/containerd"},
		{"containerdConfigFile", containerdConfigFile, "/etc/containerd/config.toml"},
		{"containerdServiceFile", containerdServiceFile, "/etc/systemd/system/containerd.service"},
		{"containerdDataDir", containerdDataDir, "/var/lib/containerd"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.value != tt.expected {
				t.Errorf("%s = %s, want %s", tt.name, tt.value, tt.expected)
			}
		})
	}
}

// TestContainerdDirs verifies containerd directories array.
// Test: Validates containerdDirs array contains required configuration directory
// Expected: Array should contain /etc/containerd directory
func TestContainerdDirs(t *testing.T) {
	if len(containerdDirs) != 1 {
		t.Errorf("Expected 1 containerd directory, got %d", len(containerdDirs))
	}

	if containerdDirs[0] != "/etc/containerd" {
		t.Errorf("containerdDirs[0] = %s, want /etc/containerd", containerdDirs[0])
	}
}

// TestContainerdBinaries verifies containerd binary names list.
// Test: Validates containerdBinaries array contains all required binaries
// Expected: Array should contain ctr, containerd, and all shim variants
func TestContainerdBinaries(t *testing.T) {
	expectedBinaries := []string{
		"ctr",
		"containerd",
		"containerd-shim",
		"containerd-shim-runc-v1",
		"containerd-shim-runc-v2",
		"containerd-stress",
	}

	if len(containerdBinaries) != len(expectedBinaries) {
		t.Errorf("Expected %d binaries, got %d", len(expectedBinaries), len(containerdBinaries))
	}

	for i, binary := range containerdBinaries {
		if binary != expectedBinaries[i] {
			t.Errorf("containerdBinaries[%d] = %s, want %s", i, binary, expectedBinaries[i])
		}
	}
}

// TestContainerdVariables verifies containerd download configuration variables.
// Test: Validates filename template and download URL template
// Expected: Variables should contain proper format specifiers for version and architecture
func TestContainerdVariables(t *testing.T) {
	if containerdFileName == "" {
		t.Error("containerdFileName should not be empty")
	}

	if containerdDownloadURL == "" {
		t.Error("containerdDownloadURL should not be empty")
	}

	expectedFileName := "containerd-%s-linux-%s.tar.gz"
	if containerdFileName != expectedFileName {
		t.Errorf("containerdFileName = %s, want %s", containerdFileName, expectedFileName)
	}
}
