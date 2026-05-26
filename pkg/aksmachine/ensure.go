package aksmachine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/Azure/unbounded/pkg/agent/phases"
)

type ensureMachineTask struct {
	machines MachineClient
	goal     GoalState
	logger   *slog.Logger
}

// EnsureMachine returns a task that ensures this machine is registered in AKS.
func EnsureMachine(machines MachineClient, goal GoalState, logger *slog.Logger) phases.Task {
	return &ensureMachineTask{machines: machines, goal: goal, logger: logger}
}

func (t *ensureMachineTask) Name() string { return "ensure-machine" }

func (t *ensureMachineTask) Do(ctx context.Context) error {
	if _, err := t.machines.Get(ctx); err == nil {
		t.logger.Info("machine already registered, skipping")
		return nil
	} else {
		var notFound *NotFoundError
		if !errors.As(err, &notFound) {
			return fmt.Errorf("ensure-machine: get machine: %w", err)
		}
	}
	if _, err := t.machines.Create(ctx, t.goal); err != nil {
		return fmt.Errorf("ensure-machine: create machine: %w", err)
	}
	return nil
}
