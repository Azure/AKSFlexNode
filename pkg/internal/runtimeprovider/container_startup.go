package runtimeprovider

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

const containerStartupFileMode = 0o600

// Preparing fences agent activation during the SDK upgrade. An interruption or
// SDK error requires operator recovery; the record cannot identify a safe slot.
// Starting records a successful SDK return with Flex completion still pending,
// not a pre-kubelet publication. Finished records completed local work.
type ContainerStartupStore struct{ Path string }

func defaultContainerStartupStore() ContainerStartupStore {
	return ContainerStartupStore{Path: filepath.Join(config.ConfigDir, containerStartupFileName)}
}

func (s ContainerStartupStore) Load() (*ContainerStartup, error) {
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read container startup: %w", err)
	}
	var startup ContainerStartup
	if err := json.Unmarshal(data, &startup); err != nil {
		return nil, fmt.Errorf("decode container startup: %w", err)
	}
	if !ValidContainerState(startup.Next) || !ValidContainerState(startup.Previous) ||
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

func (s ContainerStartupStore) Save(_ context.Context, startup *ContainerStartup) error {
	data, err := json.Marshal(startup)
	if err != nil {
		return fmt.Errorf("encode container startup: %w", err)
	}
	if err := utilio.WriteFile(s.Path, append(data, '\n'), containerStartupFileMode); err != nil {
		return fmt.Errorf("publish container startup: %w", err)
	}
	return nil
}

func (s ContainerStartupStore) Delete() error {
	if err := os.Remove(s.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove container startup: %w", err)
	}
	return nil
}
