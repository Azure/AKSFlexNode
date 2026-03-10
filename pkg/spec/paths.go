package spec

import (
	"os"
	"path/filepath"
)

// ManagedClusterSpecFilePath returns the managed cluster spec snapshot file path under a provided directory.
func ManagedClusterSpecFilePath(specDir string) string {
	return filepath.Join(specDir, "managedcluster-spec.json")
}

// GetSpecDir returns the appropriate directory for spec artifacts.
// Uses /run/aks-flex-node when running as systemd service (RuntimeDirectory creates this)
// Uses /tmp/aks-flex-node for direct user execution (testing/development)
func GetSpecDir() string {
	// Check if /run/aks-flex-node exists (created by systemd RuntimeDirectory directive)
	runtimeDir := "/run/aks-flex-node"
	if fi, err := os.Stat(runtimeDir); err == nil && fi.IsDir() {
		return runtimeDir
	}
	// Fallback to temp directory for testing/development
	return "/tmp/aks-flex-node"
}

// GetManagedClusterSpecFilePath returns the path where the managed cluster spec snapshot is stored.
func GetManagedClusterSpecFilePath() string {
	return ManagedClusterSpecFilePath(GetSpecDir())
}
