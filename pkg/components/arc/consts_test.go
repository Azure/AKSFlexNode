package arc

import (
	"testing"
)

// TestRoleDefinitionIDs verifies Azure role definition ID mappings.
// Test: Validates roleDefinitionIDs map contains all required Azure roles with correct GUIDs
// Expected: Map should contain Reader, Contributor, Network Contributor, and AKS admin roles
func TestRoleDefinitionIDs(t *testing.T) {
	expectedRoles := map[string]string{
		"Reader":              "acdd72a7-3385-48ef-bd42-f606fba81ae7",
		"Network Contributor": "4d97b98b-1d4f-4787-a291-c67834d212e7",
		"Contributor":         "b24988ac-6180-42a0-ab88-20f7382dd24c",
		"Azure Kubernetes Service RBAC Cluster Admin": "b1ff04bb-8a4e-4dc4-8eb5-8693973ce19b",
		"Azure Kubernetes Service Cluster Admin Role": "0ab0b1a8-8aac-4efd-b8c2-3ee1fb270be8",
	}

	if len(roleDefinitionIDs) != len(expectedRoles) {
		t.Errorf("Expected %d role definitions, got %d", len(expectedRoles), len(roleDefinitionIDs))
	}

	for role, id := range expectedRoles {
		if roleDefinitionIDs[role] != id {
			t.Errorf("roleDefinitionIDs[%s] = %s, want %s", role, roleDefinitionIDs[role], id)
		}
	}
}

// TestArcServices verifies Azure Arc service names list.
// Test: Validates arcServices array contains all required Arc services
// Expected: Array should contain himdsd, gcarcservice, and extd services
func TestArcServices(t *testing.T) {
	expectedServices := []string{"himdsd", "gcarcservice", "extd"}

	if len(arcServices) != len(expectedServices) {
		t.Errorf("Expected %d arc services, got %d", len(expectedServices), len(arcServices))
	}

	for i, service := range expectedServices {
		if arcServices[i] != service {
			t.Errorf("arcServices[%d] = %s, want %s", i, arcServices[i], service)
		}
	}
}

// TestRoleDefinitionIDsAreGUIDs verifies role definition IDs are valid GUIDs.
// Test: Checks all role definition IDs have correct GUID format (36 chars with dashes)
// Expected: All IDs should be properly formatted GUIDs with dashes at positions 8, 13, 18, 23
func TestRoleDefinitionIDsAreGUIDs(t *testing.T) {
	// Test that all role definition IDs are in GUID format
	for role, id := range roleDefinitionIDs {
		if len(id) != 36 {
			t.Errorf("Role %s has ID with wrong length: %d (expected 36)", role, len(id))
		}

		// Check for correct dashes
		if id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
			t.Errorf("Role %s has ID with incorrect GUID format: %s", role, id)
		}
	}
}
