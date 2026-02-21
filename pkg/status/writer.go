package status

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// WriteStatusToFile persists the node status snapshot to a JSON file.
// It writes to a temporary file and renames it for atomicity.
func WriteStatusToFile(path string, nodeStatus *NodeStatus) error {
	if path == "" {
		return fmt.Errorf("status path is empty")
	}
	if nodeStatus == nil {
		return fmt.Errorf("node status is nil")
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("failed to create status directory: %w", err)
	}

	statusData, err := json.MarshalIndent(nodeStatus, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal status to JSON: %w", err)
	}

	tempFile := path + ".tmp"
	if err := os.WriteFile(tempFile, statusData, 0o600); err != nil {
		return fmt.Errorf("failed to write status to temp file: %w", err)
	}
	if err := os.Rename(tempFile, path); err != nil {
		return fmt.Errorf("failed to rename temp status file: %w", err)
	}
	return nil
}
