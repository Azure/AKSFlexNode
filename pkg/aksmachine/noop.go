package aksmachine

import (
	"context"

	"github.com/Azure/AKSFlexNode/pkg/config"
)

const noopResourceID = "noop-machine"

type noopClient struct {
	goal GoalState
}

// NewNoopClient returns a temporary no-op machine client used until the AKS RP
// machine implementation is available.
func NewNoopClient(cfg *config.Config) MachineClient {
	version := "initial"
	if cfg != nil && cfg.Components.Kubernetes != "" {
		version = cfg.Components.Kubernetes
	}
	return &noopClient{goal: GoalState{KubernetesVersion: version, SettingsVersion: version}}
}

func (c *noopClient) Create(context.Context, GoalState) (*Machine, error) {
	return c.machine(), nil
}

func (c *noopClient) Get(context.Context) (*Machine, error) {
	return c.machine(), nil
}

func (c *noopClient) PatchStatus(context.Context, Status) error {
	return nil
}

func (c *noopClient) machine() *Machine {
	return &Machine{ID: noopResourceID, Goal: c.goal}
}
