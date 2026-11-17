package daemon

import (
	"fmt"
)

type containerStartupReader interface {
	Load() (*containerStartup, error)
}

// Applied state can still name the stopped source while the frozen target is
// running.
func requireNoContainerStartupForAgentActivation(startup containerStartupReader) error {
	pending, err := startup.Load()
	if err != nil {
		return fmt.Errorf("inspect container startup before agent activation: %w", err)
	}
	if pending != nil {
		return fmt.Errorf("agent activation requires container startup to complete (phase %s)", pending.Phase)
	}
	return nil
}
