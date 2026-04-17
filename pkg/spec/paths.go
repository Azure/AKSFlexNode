package spec

import (
	"fmt"
	"os"
)

// RuntimeDir is the single runtime directory used for spec and status artifacts.
// Under systemd, RuntimeDirectory=aks-flex-node creates this before ExecStart.
// For CLI invocations, call EnsureRuntimeDir early to create it.
const RuntimeDir = "/run/aks-flex-node"

// StatusFilePath is the path to the node status snapshot file.
const StatusFilePath = RuntimeDir + "/status.json"

// ManagedClusterSpecPath is the path to the managed cluster spec snapshot file.
const ManagedClusterSpecPath = RuntimeDir + "/managedcluster-spec.json"

// EnsureRuntimeDir creates the runtime directory with restricted permissions.
// Call this once at process startup before any code that reads or writes
// spec/status files. Returns an error if the directory cannot be created.
func EnsureRuntimeDir() error {
	if err := os.MkdirAll(RuntimeDir, 0700); err != nil {
		return fmt.Errorf("failed to create runtime directory %s: %w", RuntimeDir, err)
	}
	return nil
}
