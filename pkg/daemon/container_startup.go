package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
)

const containerStartupFileName = "container-startup.json"

// A preparing record fences crashes before the backend can expose the target
// kubelet. Only its pre-start publication callback may advance it to starting.
// Finished stays until Kubernetes observes this exact registration.
type containerStartup struct {
	Phase    string `json:"phase"`
	Next     *State `json:"next"`
	Previous *State `json:"previous"`
}

type containerStartupStore struct{ path string }

func defaultContainerStartupStore() containerStartupStore {
	return containerStartupStore{path: filepath.Join(config.ConfigDir, containerStartupFileName)}
}

func (s containerStartupStore) Load() (*containerStartup, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read container startup: %w", err)
	}
	var record noneStateRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("decode container startup record: %w", err)
	}
	if record.Version != 1 || record.Checksum != checksum(record.Payload) {
		return nil, fmt.Errorf("invalid container startup record integrity or version")
	}
	var startup containerStartup
	if err := json.Unmarshal(record.Payload, &startup); err != nil {
		return nil, fmt.Errorf("decode container startup: %w", err)
	}
	if !validContainerState(startup.Next) || !validContainerState(startup.Previous) ||
		startup.Next.ActiveMachine == startup.Previous.ActiveMachine || startup.Next.AppliedSettingsVersion == "" ||
		startup.Next.NodeConfig.Kubelet.Labels[noneGenerationLabel] == "" {
		return nil, fmt.Errorf("container startup has invalid frozen configuration or slots")
	}
	switch startup.Phase {
	case "preparing", "starting", "finished":
		return &startup, nil
	default:
		return nil, fmt.Errorf("invalid container startup phase %q", startup.Phase)
	}
}

func (s containerStartupStore) Save(_ context.Context, startup *containerStartup) error {
	payload, err := json.Marshal(startup)
	if err != nil {
		return fmt.Errorf("encode container startup: %w", err)
	}
	data, err := json.Marshal(noneStateRecord{Version: 1, Payload: payload, Checksum: checksum(payload)})
	if err != nil {
		return err
	}
	if err := os.Chmod(s.path, stateFileMode); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := utilio.WriteFileDurable(s.path, append(data, '\n'), stateFileMode); err != nil {
		return fmt.Errorf("publish container startup: %w", err)
	}
	return nil
}

func (s containerStartupStore) Delete() error {
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove container startup: %w", err)
	}
	return utilio.SyncDir(filepath.Dir(s.path))
}
