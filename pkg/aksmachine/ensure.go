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
// The task updates goal.SettingsVersion from the ETag returned by AKS so the
// initial daemon state records the remote version of the locally applied goal.
func EnsureMachine(machines MachineClient, goal *GoalState, require bool, logger *slog.Logger) phases.Task {
	return &ensureMachineTask{machines: machines, goal: goal, require: require, logger: logger}
}

func (t *ensureMachineTask) Name() string { return "ensure-machine" }

func (t *ensureMachineTask) Do(ctx context.Context) error {
	machine, err := t.machines.Get(ctx)
	if err == nil {
		if machine != nil && machine.Goal.KubernetesVersion == t.goal.KubernetesVersion {
			t.logger.Info("machine already registered, skipping")
			return t.adoptSettingsVersion(machine, "get machine")
		}

		remoteVersion := ""
		if machine != nil {
			remoteVersion = machine.Goal.KubernetesVersion
		}
		t.logger.Info(
			"updating registered machine from local bootstrap config",
			"remoteKubernetesVersion", remoteVersion,
			"localKubernetesVersion", t.goal.KubernetesVersion,
		)
		machine, err = t.machines.Create(ctx, *t.goal)
		if err != nil {
			return t.handleError("update machine", err)
		}
		return t.adoptSettingsVersion(machine, "update machine")
	}

	var notFound *NotFoundError
	if !errors.As(err, &notFound) {
		return t.handleError("get machine", err)
	}
	machine, err = t.machines.Create(ctx, *t.goal)
	if err != nil {
		return t.handleError("create machine", err)
	}
	return t.adoptSettingsVersion(machine, "create machine")
}

func (t *ensureMachineTask) adoptSettingsVersion(machine *Machine, operation string) error {
	if machine == nil {
		return t.handleError(operation, fmt.Errorf("AKS returned a nil machine"))
	}
	if machine.Goal.KubernetesVersion != t.goal.KubernetesVersion {
		return t.handleError(
			operation,
			fmt.Errorf(
				"AKS machine Kubernetes version %q does not match local bootstrap version %q",
				machine.Goal.KubernetesVersion,
				t.goal.KubernetesVersion,
			),
		)
	}
	if machine.Goal.SettingsVersion != "" {
		t.goal.SettingsVersion = machine.Goal.SettingsVersion
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
