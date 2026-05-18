package config

import (
	"fmt"
	"os"
)

const RuntimeDir = "/run/aks-flex-node"

// EnsureRuntimeDir creates the runtime directory for CLI invocations.
func EnsureRuntimeDir() error {
	if err := os.MkdirAll(RuntimeDir, 0o700); err != nil {
		return fmt.Errorf("failed to create runtime directory %s: %w", RuntimeDir, err)
	}
	return nil
}
