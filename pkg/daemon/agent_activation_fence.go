package daemon

import (
	"context"
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

type agentActivationStateLoader struct {
	startup containerStartupReader
	load    func(context.Context) (*State, error)
}

func (s agentActivationStateLoader) LoadState(ctx context.Context) (*State, error) {
	if err := requireNoContainerStartupForAgentActivation(s.startup); err != nil {
		return nil, err
	}
	state, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	// Direct activation runs outside the controller queue: a startup may have
	// been published while applied state was being read.
	if err := requireNoContainerStartupForAgentActivation(s.startup); err != nil {
		return nil, err
	}
	return state, nil
}
