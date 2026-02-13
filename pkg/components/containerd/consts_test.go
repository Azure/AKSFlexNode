package containerd

import (
	"reflect"
	"testing"
)

func TestGetMajorVersion(t *testing.T) {
	tests := []struct {
		name     string
		version  string
		expected int
	}{
		{
			name:     "containerd 1.x version",
			version:  "1.7.20",
			expected: 1,
		},
		{
			name:     "containerd 2.x version",
			version:  "2.0.0",
			expected: 2,
		},
		{
			name:     "containerd 2.x with patch",
			version:  "2.1.5",
			expected: 2,
		},
		{
			name:     "empty version",
			version:  "",
			expected: 0,
		},
		{
			name:     "invalid version",
			version:  "invalid",
			expected: 0,
		},
		{
			name:     "single digit version",
			version:  "1",
			expected: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getMajorVersion(tt.version)
			if result != tt.expected {
				t.Errorf("getMajorVersion(%q) = %d, want %d", tt.version, result, tt.expected)
			}
		})
	}
}

func TestGetContainerdBinariesForVersion(t *testing.T) {
	tests := []struct {
		name     string
		version  string
		expected []string
	}{
		{
			name:    "containerd 1.x includes all binaries",
			version: "1.7.20",
			expected: []string{
				"ctr",
				"containerd",
				"containerd-shim",
				"containerd-shim-runc-v1",
				"containerd-shim-runc-v2",
				"containerd-stress",
			},
		},
		{
			name:    "containerd 2.x excludes deprecated shim binaries",
			version: "2.0.0",
			expected: []string{
				"ctr",
				"containerd",
				"containerd-shim-runc-v2",
				"containerd-stress",
			},
		},
		{
			name:    "containerd 2.1.x excludes deprecated shim binaries",
			version: "2.1.5",
			expected: []string{
				"ctr",
				"containerd",
				"containerd-shim-runc-v2",
				"containerd-stress",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getContainerdBinariesForVersion(tt.version)
			if !reflect.DeepEqual(result, tt.expected) {
				t.Errorf("getContainerdBinariesForVersion(%q) = %v, want %v", tt.version, result, tt.expected)
			}
		})
	}
}

func TestGetContainerdBinariesForVersion_ExcludesDeprecated(t *testing.T) {
	// Test that containerd 2.x explicitly excludes the deprecated binaries
	v2Binaries := getContainerdBinariesForVersion("2.0.0")

	v1OnlyBinaries := []string{"containerd-shim", "containerd-shim-runc-v1"}
	for _, v1Only := range v1OnlyBinaries {
		for _, binary := range v2Binaries {
			if binary == v1Only {
				t.Errorf("containerd 2.x should not include v1-only binary: %s", v1Only)
			}
		}
	}

	// Verify containerd-shim-runc-v2 is still included
	found := false
	for _, binary := range v2Binaries {
		if binary == "containerd-shim-runc-v2" {
			found = true
			break
		}
	}
	if !found {
		t.Error("containerd 2.x should include containerd-shim-runc-v2")
	}
}

func TestGetAllContainerdBinaries(t *testing.T) {
	allBinaries := getAllContainerdBinaries()

	// Should include all v1 binaries
	expectedV1 := []string{"ctr", "containerd", "containerd-shim", "containerd-shim-runc-v1", "containerd-shim-runc-v2", "containerd-stress"}
	for _, expected := range expectedV1 {
		found := false
		for _, binary := range allBinaries {
			if binary == expected {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("getAllContainerdBinaries() should include v1 binary: %s", expected)
		}
	}

	// Should not have duplicates
	seen := make(map[string]bool)
	for _, binary := range allBinaries {
		if seen[binary] {
			t.Errorf("getAllContainerdBinaries() contains duplicate: %s", binary)
		}
		seen[binary] = true
	}
}
