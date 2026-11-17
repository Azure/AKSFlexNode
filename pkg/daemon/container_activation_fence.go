package daemon

import (
	"fmt"
)

type containerStartupReader interface {
	Load() (*containerStartup, error)
}

// Applied state can still name the stopped source while the frozen target is
// running. Even finished startups retain the fence until registration is observed.
func requireNoContainerStartupForAgentActivation(startup containerStartupReader) error {
	pending, err := startup.Load()
	if err != nil {
		return fmt.Errorf("inspect container startup before agent activation: %w", err)
	}
	if pending != nil {
		return fmt.Errorf("agent activation requires container startup to complete and its registration to be observed (phase %s)", pending.Phase)
	}
	return nil
}
