package status

import (
	"encoding/json"
	"fmt"

	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
)

// WriteStatusToFile persists the node status snapshot to a JSON file.
// It writes atomically to avoid partial writes.
func WriteStatusToFile(path string, nodeStatus *NodeStatus) error {
	return withStatusFileLock(func() error {
		return writeStatusToFileUnlocked(path, nodeStatus)
	})
}

func writeStatusToFileUnlocked(path string, nodeStatus *NodeStatus) error {
	if path == "" {
		return fmt.Errorf("status path is empty")
	}
	if nodeStatus == nil {
		return fmt.Errorf("node status is nil")
	}

	statusData, err := json.MarshalIndent(nodeStatus, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal status to JSON: %w", err)
	}

	if err := utilio.WriteFile(path, statusData, 0o600); err != nil {
		return fmt.Errorf("failed to write status file: %w", err)
	}
	return nil
}
