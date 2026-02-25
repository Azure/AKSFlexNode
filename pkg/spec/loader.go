package spec

import (
	"encoding/json"
	"fmt"
	"os"
)

// LoadManagedClusterSpec loads the managed cluster spec snapshot from the default path.
func LoadManagedClusterSpec() (*ManagedClusterSpec, error) {
	return LoadManagedClusterSpecFromFile(GetManagedClusterSpecFilePath())
}

// LoadManagedClusterSpecFromFile loads the managed cluster spec snapshot from a JSON file.
func LoadManagedClusterSpecFromFile(path string) (*ManagedClusterSpec, error) {
	if path == "" {
		return nil, fmt.Errorf("spec path is empty")
	}

	// #nosec G304 -- reading a local snapshot file path controlled by the agent (runtime/temp dir), not user input.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var s ManagedClusterSpec
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("failed to unmarshal managed cluster spec: %w", err)
	}

	return &s, nil
}
