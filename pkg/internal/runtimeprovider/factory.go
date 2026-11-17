package runtimeprovider

import (
	"context"
	"fmt"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

var implementations = map[isolation.Mode]func(HostServices) NodeProvider{
	isolation.ModeNSpawn: func(host HostServices) NodeProvider {
		return NSpawn{Host: host, BinaryPath: NSpawnAgentBinaryPath}
	},
	isolation.ModeNone: func(host HostServices) NodeProvider {
		return None{Host: host}
	},
}

// New constructs the runtime for an explicitly selected mode.
func New(mode isolation.Mode, host HostServices) (NodeProvider, error) {
	construct, ok := implementations[mode]
	if !ok {
		return nil, fmt.Errorf("unsupported node isolation %q", mode)
	}
	return construct(host), nil
}

// All constructs the installed-runtime candidates used during discovery.
func All(host HostServices) []NodeProvider {
	providers := make([]NodeProvider, 0, len(implementations))
	for _, construct := range implementations {
		providers = append(providers, construct(host))
	}
	return providers
}

// ValidateAppliedState rejects mode changes without a reset.
func ValidateAppliedState(provider NodeProvider, applied *State) error {
	if applied != nil && applied.Isolation != provider.Mode() {
		return fmt.Errorf("configured isolation %s differs from applied isolation %s; reset the node before changing modes", provider.Mode(), applied.Isolation)
	}
	return nil
}

func validActiveMachine(machine string) bool {
	return machine == goalstates.NSpawnMachineKube1 || machine == goalstates.NSpawnMachineKube2
}

func activeMachineFromStore(ctx context.Context, store StateStore) (*activeMachine, error) {
	state, err := store.Load(ctx)
	if err != nil {
		return nil, err
	}
	if state == nil {
		return nil, fmt.Errorf("daemon state is missing active machine")
	}
	if !validActiveMachine(state.ActiveMachine) {
		return nil, fmt.Errorf("daemon state active machine %q is invalid", state.ActiveMachine)
	}
	return &activeMachine{Name: state.ActiveMachine, State: state}, nil
}
