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
// Local configuration remains authoritative during bootstrap. When the remote
// Kubernetes version already matches, the task adopts only the remote ETag as
// the reconciliation baseline; other remote settings do not replace the local
// goal. Subsequent ETag changes are handled by the daemon as new remote goals.
func EnsureMachine(machines MachineClient, goal *GoalState, require bool, logger *slog.Logger) phases.Task {
	return &ensureMachineTask{machines: machines, goal: goal, require: require, logger: logger}
}

func (t *ensureMachineTask) Name() string { return "ensure-machine" }

func (t *ensureMachineTask) Do(ctx context.Context) error {
	remoteMachine, err := t.fetchRemoteMachine(ctx)
	if err != nil {
		return t.handleError("get machine", err)
	}

	switch {
	case remoteMachine == nil:
		remoteMachine, err = t.createRemoteMachineFromGoal(ctx)
		if err != nil {
			return t.handleError("create machine", err)
		}
	case machineGoalHasDrift(remoteMachine.Goal, *t.goal):
		remoteMachine, err = t.updateRemoteMachineFromGoal(ctx, remoteMachine)
		if err != nil {
			return t.handleError("update machine", err)
		}
	default:
		t.logger.Info("machine already registered, skipping")
	}
	t.applyGoalStateWithRemoteMachineSettingsVersion(remoteMachine)
	return nil
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
	if err := validateMachineForGoal(machine, *t.goal); err != nil {
		return nil, err
	}
	return machine, nil
}

func (t *ensureMachineTask) updateRemoteMachineFromGoal(ctx context.Context, current *Machine) (*Machine, error) {
	t.logger.Info(
		"updating registered machine from local bootstrap config",
		"remoteKubernetesVersion", current.Goal.KubernetesVersion,
		"localKubernetesVersion", t.goal.KubernetesVersion,
	)
	machine, err := t.machines.Create(ctx, *t.goal)
	if err != nil {
		return nil, err
	}
	if err := validateMachineForGoal(machine, *t.goal); err != nil {
		return nil, err
	}
	return machine, nil
}

func (t *ensureMachineTask) applyGoalStateWithRemoteMachineSettingsVersion(machine *Machine) {
	t.goal.SettingsVersion = machine.Goal.SettingsVersion
}

func machineGoalHasDrift(remote, desired GoalState) bool {
	return remote.KubernetesVersion != desired.KubernetesVersion
}

func validateMachineForGoal(machine *Machine, goal GoalState) error {
	if err := machine.Validate(); err != nil {
		return fmt.Errorf("AKS returned an invalid machine: %w", err)
	}
	if machine.Goal.KubernetesVersion != goal.KubernetesVersion {
		return fmt.Errorf(
			"AKS machine Kubernetes version %q does not match local bootstrap version %q",
			machine.Goal.KubernetesVersion,
			goal.KubernetesVersion,
		)
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
