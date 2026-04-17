package status

import (
	"encoding/json"
	"testing"
	"time"
)

// TestNodeStatus verifies NodeStatus structure serialization and deserialization.
// Test: Creates a NodeStatus with all fields populated, marshals to JSON, then unmarshals back
// Expected: All fields (versions, running status, Arc status) should round-trip correctly through JSON
func TestNodeStatus(t *testing.T) {
	now := time.Now()

	status := &NodeStatus{
		KubeletVersion:    "v1.26.0",
		RuncVersion:       "1.1.12",
		ContainerdVersion: "1.7.0",
		KubeletRunning:    true,
		KubeletReady:      "True",
		ContainerdRunning: true,
		ArcStatus: ArcStatus{
			Registered:    true,
			Connected:     true,
			MachineName:   "test-machine",
			ResourceID:    "/subscriptions/test/resourceGroups/test-rg/providers/Microsoft.HybridCompute/machines/test-machine",
			Location:      "eastus",
			ResourceGroup: "test-rg",
			LastHeartbeat: now,
			AgentVersion:  "1.0.0",
		},
		LastUpdated:  now,
		AgentVersion: "dev",
	}

	// Test JSON marshaling
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("Failed to marshal NodeStatus: %v", err)
	}

	// Test JSON unmarshaling
	var unmarshaled NodeStatus
	if err := json.Unmarshal(data, &unmarshaled); err != nil {
		t.Fatalf("Failed to unmarshal NodeStatus: %v", err)
	}

	// Verify key fields
	if unmarshaled.KubeletVersion != status.KubeletVersion {
		t.Errorf("KubeletVersion mismatch: got %s, want %s", unmarshaled.KubeletVersion, status.KubeletVersion)
	}

	if unmarshaled.RuncVersion != status.RuncVersion {
		t.Errorf("RuncVersion mismatch: got %s, want %s", unmarshaled.RuncVersion, status.RuncVersion)
	}

	if unmarshaled.ContainerdVersion != status.ContainerdVersion {
		t.Errorf("ContainerdVersion mismatch: got %s, want %s", unmarshaled.ContainerdVersion, status.ContainerdVersion)
	}

	if unmarshaled.KubeletRunning != status.KubeletRunning {
		t.Errorf("KubeletRunning mismatch: got %v, want %v", unmarshaled.KubeletRunning, status.KubeletRunning)
	}

	if unmarshaled.ContainerdRunning != status.ContainerdRunning {
		t.Errorf("ContainerdRunning mismatch: got %v, want %v", unmarshaled.ContainerdRunning, status.ContainerdRunning)
	}

	if unmarshaled.AgentVersion != status.AgentVersion {
		t.Errorf("AgentVersion mismatch: got %s, want %s", unmarshaled.AgentVersion, status.AgentVersion)
	}
}

// TestArcStatus verifies ArcStatus structure in different connection states.
// Test: Tests Arc status in various states (registered+connected, registered only, not registered)
// Expected: All Arc status fields should serialize/deserialize correctly in all states
func TestArcStatus(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name   string
		status ArcStatus
	}{
		{
			name: "fully registered and connected",
			status: ArcStatus{
				Registered:    true,
				Connected:     true,
				MachineName:   "test-machine",
				ResourceID:    "/subscriptions/test/resourceGroups/test-rg/providers/Microsoft.HybridCompute/machines/test-machine",
				Location:      "eastus",
				ResourceGroup: "test-rg",
				LastHeartbeat: now,
				AgentVersion:  "1.0.0",
			},
		},
		{
			name: "registered but not connected",
			status: ArcStatus{
				Registered:    true,
				Connected:     false,
				MachineName:   "test-machine",
				ResourceID:    "/subscriptions/test/resourceGroups/test-rg/providers/Microsoft.HybridCompute/machines/test-machine",
				Location:      "eastus",
				ResourceGroup: "test-rg",
				AgentVersion:  "1.0.0",
			},
		},
		{
			name: "not registered",
			status: ArcStatus{
				Registered: false,
				Connected:  false,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test JSON marshaling
			data, err := json.Marshal(tt.status)
			if err != nil {
				t.Fatalf("Failed to marshal ArcStatus: %v", err)
			}

			// Test JSON unmarshaling
			var unmarshaled ArcStatus
			if err := json.Unmarshal(data, &unmarshaled); err != nil {
				t.Fatalf("Failed to unmarshal ArcStatus: %v", err)
			}

			// Verify key fields
			if unmarshaled.Registered != tt.status.Registered {
				t.Errorf("Registered mismatch: got %v, want %v", unmarshaled.Registered, tt.status.Registered)
			}

			if unmarshaled.Connected != tt.status.Connected {
				t.Errorf("Connected mismatch: got %v, want %v", unmarshaled.Connected, tt.status.Connected)
			}

			if unmarshaled.MachineName != tt.status.MachineName {
				t.Errorf("MachineName mismatch: got %s, want %s", unmarshaled.MachineName, tt.status.MachineName)
			}
		})
	}
}

// TestNodeStatus_EmptyStatus verifies empty NodeStatus handles default values correctly.
// Test: Creates empty NodeStatus, marshals and unmarshals
// Expected: Boolean fields should default to false, serialization should succeed
func TestNodeStatus_EmptyStatus(t *testing.T) {
	status := &NodeStatus{}

	// Test that empty status can be marshaled
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("Failed to marshal empty NodeStatus: %v", err)
	}

	// Test that it can be unmarshaled back
	var unmarshaled NodeStatus
	if err := json.Unmarshal(data, &unmarshaled); err != nil {
		t.Fatalf("Failed to unmarshal empty NodeStatus: %v", err)
	}

	// Verify defaults
	if unmarshaled.KubeletRunning {
		t.Error("Empty status should have KubeletRunning as false")
	}

	if unmarshaled.ContainerdRunning {
		t.Error("Empty status should have ContainerdRunning as false")
	}
}

// TestNodeStatus_JSONFieldNames verifies JSON field names match expected camelCase format.
// Test: Marshals NodeStatus to JSON and checks field names in output
// Expected: All fields should use camelCase naming (kubeletVersion, not KubeletVersion)
func TestNodeStatus_JSONFieldNames(t *testing.T) {
	status := &NodeStatus{
		KubeletVersion:    "v1.26.0",
		RuncVersion:       "1.1.12",
		ContainerdVersion: "1.7.0",
		KubeletRunning:    true,
		KubeletReady:      "True",
		ContainerdRunning: true,
		AgentVersion:      "dev",
		LastUpdated:       time.Now(),
	}

	data, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("Failed to marshal NodeStatus: %v", err)
	}

	// Unmarshal to map to check JSON field names
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("Failed to unmarshal to map: %v", err)
	}

	// Check that expected fields are present with correct JSON names
	expectedFields := []string{
		"kubeletVersion",
		"runcVersion",
		"containerdVersion",
		"kubeletRunning",
		"kubeletReady",
		"containerdRunning",
		"arcStatus",
		"lastUpdated",
		"agentVersion",
	}

	for _, field := range expectedFields {
		if _, exists := m[field]; !exists {
			t.Errorf("Expected field %s not found in JSON output", field)
		}
	}
}
