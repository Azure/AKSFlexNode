package cni

import (
	"testing"
)

// TestCNIConstants verifies CNI configuration path and version constants.
// Test: Validates CNI directories, config file names, plugin names, and version strings
// Expected: All paths should match standard CNI installation locations and specifications
func TestCNIConstants(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		expected string
	}{
		{"DefaultCNIBinDir", DefaultCNIBinDir, "/opt/cni/bin"},
		{"DefaultCNIConfDir", DefaultCNIConfDir, "/etc/cni/net.d"},
		{"DefaultCNILibDir", DefaultCNILibDir, "/var/lib/cni"},
		{"bridgeConfigFile", bridgeConfigFile, "10-bridge.conf"},
		{"bridgePlugin", bridgePlugin, "bridge"},
		{"hostLocalPlugin", hostLocalPlugin, "host-local"},
		{"loopbackPlugin", loopbackPlugin, "loopback"},
		{"portmapPlugin", portmapPlugin, "portmap"},
		{"bandwidthPlugin", bandwidthPlugin, "bandwidth"},
		{"tuningPlugin", tuningPlugin, "tuning"},
		{"DefaultCNIVersion", DefaultCNIVersion, "1.5.1"},
		{"DefaultCNISpecVersion", DefaultCNISpecVersion, "0.3.1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.value != tt.expected {
				t.Errorf("%s = %s, want %s", tt.name, tt.value, tt.expected)
			}
		})
	}
}

// TestCNIDirectories verifies CNI directory paths array.
// Test: Validates cniDirs array contains all required CNI directories
// Expected: Array should contain bin, conf, and lib directories in correct order
func TestCNIDirectories(t *testing.T) {
	if len(cniDirs) != 3 {
		t.Errorf("Expected 3 CNI directories, got %d", len(cniDirs))
	}

	expectedDirs := []string{
		"/opt/cni/bin",
		"/etc/cni/net.d",
		"/var/lib/cni",
	}

	for i, dir := range cniDirs {
		if dir != expectedDirs[i] {
			t.Errorf("cniDirs[%d] = %s, want %s", i, dir, expectedDirs[i])
		}
	}
}

// TestRequiredCNIPlugins verifies required CNI plugins list.
// Test: Validates requiredCNIPlugins array contains essential plugins
// Expected: Array should contain bridge, host-local, and loopback plugins
func TestRequiredCNIPlugins(t *testing.T) {
	if len(requiredCNIPlugins) != 3 {
		t.Errorf("Expected 3 required CNI plugins, got %d", len(requiredCNIPlugins))
	}

	expectedPlugins := []string{
		"bridge",
		"host-local",
		"loopback",
	}

	for i, plugin := range requiredCNIPlugins {
		if plugin != expectedPlugins[i] {
			t.Errorf("requiredCNIPlugins[%d] = %s, want %s", i, plugin, expectedPlugins[i])
		}
	}
}

// TestCNIVariables verifies CNI download configuration variables.
// Test: Validates filename template and download URL template
// Expected: Variables should contain proper format specifiers for architecture and version
func TestCNIVariables(t *testing.T) {
	if cniFileName == "" {
		t.Error("cniFileName should not be empty")
	}

	if cniDownLoadURL == "" {
		t.Error("cniDownLoadURL should not be empty")
	}

	// Verify format
	expectedCNIFileName := "cni-plugins-linux-%s-v%s.tgz"
	if cniFileName != expectedCNIFileName {
		t.Errorf("cniFileName = %s, want %s", cniFileName, expectedCNIFileName)
	}
}
