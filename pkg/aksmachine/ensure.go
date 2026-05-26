package aksmachine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type ensureMachineTask struct {
	cfg    *config.Config
	goal   GoalState
	logger *slog.Logger
}

// EnsureMachine returns a task that ensures this machine is registered in AKS.
func EnsureMachine(cfg *config.Config, goal GoalState, logger *slog.Logger) phases.Task {
	return &ensureMachineTask{cfg: cfg, goal: goal, logger: logger}
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
	if _, err := machines.Create(ctx, t.goal); err != nil {
		return fmt.Errorf("ensure-machine: create machine: %w", err)
	}
	return nil
}
