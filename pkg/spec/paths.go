package spec

import (
	"os/user"
	"path/filepath"
)

// ManagedClusterSpecFilePath returns the managed cluster spec snapshot file path under a provided directory.
func ManagedClusterSpecFilePath(specDir string) string {
	return filepath.Join(specDir, "managedcluster-spec.json")
}

// GetSpecDir returns the appropriate directory for spec artifacts.
// Uses /run/aks-flex-node when running as aks-flex-node user (systemd service)
// Uses /tmp/aks-flex-node for direct user execution (testing/development)
func GetSpecDir() string {
	specDir := "/tmp/aks-flex-node"
	currentUser, err := user.Current()
	if err == nil && currentUser.Username == "aks-flex-node" {
		specDir = "/run/aks-flex-node"
	}
	return specDir
}

// GetManagedClusterSpecFilePath returns the path where the managed cluster spec snapshot is stored.
func GetManagedClusterSpecFilePath() string {
	return ManagedClusterSpecFilePath(GetSpecDir())
}
