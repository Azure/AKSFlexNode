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
	require  bool
}

// EnsureMachine returns a task that ensures this machine is registered in AKS.
func EnsureMachine(machines MachineClient, goal GoalState, require bool, logger *slog.Logger) phases.Task {
	return &ensureMachineTask{machines: machines, goal: goal, require: require, logger: logger}
}

func (t *ensureMachineTask) Name() string { return "ensure-machine" }

func (t *ensureMachineTask) Do(ctx context.Context) error {
	if _, err := t.machines.Get(ctx); err == nil {
		t.logger.Info("machine already registered, skipping")
		// The prior run may have registered the machine before node startup failed;
		// continue so startup can retry the node and status update later.
		return nil
	} else {
		var notFound *NotFoundError
		if !errors.As(err, &notFound) {
			return t.handleError("get machine", err)
		}
	}
	if _, err := t.machines.Create(ctx, t.goal); err != nil {
		return t.handleError("create machine", err)
	}
	return nil
}

func (t *ensureMachineTask) handleError(operation string, err error) error {
	if t.require {
		return fmt.Errorf("ensure-machine: %s: %w", operation, err)
	}
	t.logger.Warn("skipping AKS machine registration after failure", "operation", operation, "error", err)
	return nil
}
