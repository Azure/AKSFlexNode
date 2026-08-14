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
	goal     *GoalState
	logger   *slog.Logger
	require  bool
}

// EnsureMachine returns a task that ensures this machine is registered in AKS.
// Local bootstrap configuration seeds a new Machine. Once a Machine exists, its
// complete goal is authoritative for Node creation and later reconciliation.
func EnsureMachine(machines MachineClient, goal *GoalState, require bool, logger *slog.Logger) phases.Task {
	return &ensureMachineTask{machines: machines, goal: goal, require: require, logger: logger}
}

func (t *ensureMachineTask) Name() string { return "ensure-machine" }

func (t *ensureMachineTask) Do(ctx context.Context) error {
	machine, err := t.machines.Get(ctx)
	if err == nil {
		t.logger.Info("ARM machine already registered, adopting remote goal")
		return t.adoptGoal(machine, "get machine")
	}

	var notFound *NotFoundError
	if !errors.As(err, &notFound) {
		return t.handleError("get machine", err)
	}
	machine, err = t.machines.Create(ctx, *t.goal)
	if err != nil {
		return t.handleError("create machine", err)
	}
	return t.adoptGoal(machine, "create machine")
}

func (t *ensureMachineTask) adoptGoal(machine *Machine, operation string) error {
	if err := machine.Validate(); err != nil {
		return t.handleError(operation, fmt.Errorf("AKS returned an invalid machine: %w", err))
	}

	*t.goal = machine.Goal
	return nil
}

func (t *ensureMachineTask) handleError(operation string, err error) error {
	if t.require {
		return fmt.Errorf("ensure-machine: %s: %w", operation, err)
	}
	t.logger.Warn("skipping AKS machine registration after failure", "operation", operation, "error", err)
	return nil
}
