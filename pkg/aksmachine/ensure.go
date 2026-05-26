package aksmachine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

const (
	aksFlexNodePoolName = "aksflexnodes"
	flexNodeTagKey      = "aks-flex-node"
)

type ensureMachineTask struct {
	cfg    *config.Config
	logger *slog.Logger
}

// EnsureMachine returns a task that ensures this machine is registered in AKS.
func EnsureMachine(cfg *config.Config, logger *slog.Logger) phases.Task {
	return &ensureMachineTask{cfg: cfg, logger: logger}
}

func (t *ensureMachineTask) Name() string { return "ensure-machine" }

func (t *ensureMachineTask) Do(ctx context.Context) error {
	machines, err := NewARMClient(t.cfg, t.logger)
	if err != nil {
		return fmt.Errorf("ensure-machine: create ARM machine client: %w", err)
	}
	if _, err := machines.Get(ctx); err == nil {
		t.logger.Info("machine already registered, skipping", "machine", t.cfg.Agent.NodeName)
		return nil
	} else {
		var notFound *NotFoundError
		if !errors.As(err, &notFound) {
			return fmt.Errorf("ensure-machine: get machine: %w", err)
		}
	}
	goal, err := goalStateFromConfig(t.cfg)
	if err != nil {
		return fmt.Errorf("ensure-machine: build goal state: %w", err)
	}
	if _, err := machines.Create(ctx, goal); err != nil {
		return fmt.Errorf("ensure-machine: create machine: %w", err)
	}
	return nil
}
