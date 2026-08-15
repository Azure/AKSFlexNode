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
// Local configuration seeds a Machine when one does not exist. A Machine
// returned by AKS is authoritative for bootstrap and later reconciliation.
func EnsureMachine(machines MachineClient, goal *GoalState, require bool, logger *slog.Logger) phases.Task {
	return &ensureMachineTask{machines: machines, goal: goal, require: require, logger: logger}
}

func (t *ensureMachineTask) Name() string { return "ensure-machine" }

func (t *ensureMachineTask) Do(ctx context.Context) error {
	remoteMachine, err := t.fetchRemoteMachine(ctx)
	if err != nil {
		return t.handleError("get machine", err)
	}

	switch remoteMachine {
	case nil:
		remoteMachine, err = t.createRemoteMachineFromGoal(ctx)
		if err != nil {
			return t.handleError("create machine", err)
		}
	default:
		t.logger.Info("machine already registered, adopting remote goal")
	}
	return t.applyRemoteMachineGoal(remoteMachine)
}

func (t *ensureMachineTask) fetchRemoteMachine(ctx context.Context) (*Machine, error) {
	machine, err := t.machines.Get(ctx)
	var notFound *NotFoundError
	if errors.As(err, &notFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := machine.Validate(); err != nil {
		return nil, fmt.Errorf("AKS returned an invalid machine: %w", err)
	}
	return machine, nil
}

func (t *ensureMachineTask) createRemoteMachineFromGoal(ctx context.Context) (*Machine, error) {
	machine, err := t.machines.Create(ctx, *t.goal)
	if err != nil {
		return nil, err
	}
	if err := machine.Validate(); err != nil {
		return nil, fmt.Errorf("AKS returned an invalid machine: %w", err)
	}
	return machine, nil
}

func (t *ensureMachineTask) applyRemoteMachineGoal(machine *Machine) error {
	effectiveGoal, err := EffectiveGoal(machine.Goal, *t.goal)
	if err != nil {
		return t.handleError("apply machine goal", err)
	}
	*t.goal = effectiveGoal
	return nil
}

func (t *ensureMachineTask) handleError(operation string, err error) error {
	if t.require {
		return fmt.Errorf("ensure-machine: %s: %w", operation, err)
	}
	t.logger.Warn("skipping AKS machine registration after failure", "operation", operation, "error", err)
	return nil
}
