package services

import (
	"testing"
	"time"
)

// TestServicesConstants verifies service name constants for system services.
// Test: Validates containerd and kubelet service names
// Expected: Service names should match systemd service unit names (without .service extension)
func TestServicesConstants(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		expected string
	}{
		{"ContainerdService", ContainerdService, "containerd"},
		{"KubeletService", KubeletService, "kubelet"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.value != tt.expected {
				t.Errorf("%s = %s, want %s", tt.name, tt.value, tt.expected)
			}
		})
	}
}

// TestServiceStartupTimeout verifies the service startup timeout constant.
// Test: Checks ServiceStartupTimeout value
// Expected: Timeout should be 30 seconds for service startup operations
func TestServiceStartupTimeout(t *testing.T) {
	expected := 30 * time.Second
	if ServiceStartupTimeout != expected {
		t.Errorf("ServiceStartupTimeout = %v, want %v", ServiceStartupTimeout, expected)
	}
}
