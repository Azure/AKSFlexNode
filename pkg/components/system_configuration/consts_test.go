package system_configuration

import (
	"testing"
)

// TestSystemConfigurationConstants verifies system configuration file path constants.
// Test: Validates sysctl directory and configuration file paths
// Expected: Paths should match standard Linux system configuration locations
func TestSystemConfigurationConstants(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		expected string
	}{
		{"sysctlDir", sysctlDir, "/etc/sysctl.d"},
		{"sysctlConfigPath", sysctlConfigPath, "/etc/sysctl.d/999-sysctl-aks.conf"},
		{"resolvConfPath", resolvConfPath, "/etc/resolv.conf"},
		{"resolvConfSource", resolvConfSource, "/run/systemd/resolve/resolv.conf"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.value != tt.expected {
				t.Errorf("%s = %s, want %s", tt.name, tt.value, tt.expected)
			}
		})
	}
}
