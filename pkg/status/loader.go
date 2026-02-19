package status

import (
	"encoding/json"
	"fmt"
	"os"
)

// LoadStatus loads the node status snapshot from the default path.
func LoadStatus() (*NodeStatus, error) {
	return LoadStatusFromFile(GetStatusFilePath())
}

// LoadStatusFromFile loads the node status snapshot from a JSON file.
func LoadStatusFromFile(path string) (*NodeStatus, error) {
	if path == "" {
		return nil, fmt.Errorf("status path is empty")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var s NodeStatus
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("failed to unmarshal node status: %w", err)
	}

	return &s, nil
}
