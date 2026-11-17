package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/internal/runtimeprovider"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
)

const containerStartupFileName = "container-startup.json"

// Preparing fences agent activation during the SDK upgrade. An interruption or
// SDK error requires operator recovery; the record cannot identify a safe slot.
// Starting records a successful SDK return with Flex completion still pending,
// not a pre-kubelet publication. Finished records completed local work.
type containerStartup = runtimeprovider.ContainerStartup

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
	var startup containerStartup
	if err := json.Unmarshal(data, &startup); err != nil {
		return nil, fmt.Errorf("decode container startup: %w", err)
	}
	if !runtimeprovider.ValidContainerState(startup.Next) || !runtimeprovider.ValidContainerState(startup.Previous) ||
		startup.Next.ActiveMachine == startup.Previous.ActiveMachine || startup.Next.AppliedSettingsVersion == "" {
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
	data, err := json.Marshal(startup)
	if err != nil {
		return fmt.Errorf("encode container startup: %w", err)
	}
	if err := utilio.WriteFile(s.path, append(data, '\n'), stateFileMode); err != nil {
		return fmt.Errorf("publish container startup: %w", err)
	}
	return nil
}

func (s containerStartupStore) Delete() error {
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove container startup: %w", err)
	}
	return nil
}
