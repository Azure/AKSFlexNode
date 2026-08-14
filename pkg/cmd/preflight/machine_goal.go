package preflight

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

const machineGoalCheckName = "AKSMachineGoal"

type machineGoalCheck struct {
	remoteGoal *aksmachine.GoalState
	result     preflight.Result
	log        *slog.Logger
	err        error
}

func (c machineGoalCheck) Name() string { return c.result.Name }

func (c machineGoalCheck) Check(context.Context) []preflight.Result {
	if c.err != nil {
		c.log.Warn("preflight could not use AKS Machine goal", "error", c.err)
	}
	return preflight.Results(c.result)
}

// newMachineGoalCheck resolves the authoritative goal before the other preflight
// checks are created so they validate the same inputs that bootstrap will use.
func newMachineGoalCheck(
	ctx context.Context,
	log *slog.Logger,
	cfg *config.Config,
) (*machineGoalCheck, error) {
	localGoal, err := aksmachine.GoalStateFromConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build goal state from config: %w", err)
	}
	machines, err := aksmachine.NewMachineClient(cfg, log, aksmachine.MachineClientOptions{})
	if err != nil {
		return nil, fmt.Errorf("create AKS machine client: %w", err)
	}

	requireMachineRegistration := cfg.Agent.RequireMachineRegistration != nil && *cfg.Agent.RequireMachineRegistration
	remoteGoal, result, err := resolveMachineGoal(ctx, machines, localGoal, requireMachineRegistration)
	return &machineGoalCheck{remoteGoal: remoteGoal, result: result, log: log, err: err}, nil
}

// resolveMachineGoal adopts an existing Machine read-only so the remaining
// checks validate the bootstrap inputs derived from the same remote goal as start.
func resolveMachineGoal(
	ctx context.Context,
	machines aksmachine.MachineClient,
	localGoal aksmachine.GoalState,
	requireMachineRegistration bool,
) (*aksmachine.GoalState, preflight.Result, error) {
	machine, err := machines.Get(ctx)
	if err != nil {
		var notFound *aksmachine.NotFoundError
		if errors.As(err, &notFound) {
			// A nil remote goal preserves the original config-only resolution path.
			return nil, preflight.OK(
				machineGoalCheckName,
				"AKS Machine goal",
				"AKS Machine does not exist; validating the local bootstrap goal",
			), nil
		}
		return machineGoalFailure(requireMachineRegistration, "read AKS Machine", err)
	}
	if err := machine.Validate(); err != nil {
		return machineGoalFailure(requireMachineRegistration, "validate AKS Machine", err)
	}

	if !goalsMatch(machine.Goal, localGoal) {
		return &machine.Goal, preflight.Error(
			machineGoalCheckName,
			"AKS Machine goal",
			"existing AKS Machine goal is authoritative and differs from local config; validating bootstrap inputs derived from the AKS Machine goal",
		), nil
	}

	return &machine.Goal, preflight.OK(
		machineGoalCheckName,
		"AKS Machine goal",
		"existing AKS Machine goal is authoritative and matches local config; validating bootstrap inputs derived from the AKS Machine goal",
	), nil
}

func goalsMatch(remote, local aksmachine.GoalState) bool {
	normalize := func(goal aksmachine.GoalState) aksmachine.GoalState {
		// SettingsVersion is the remote ETag and has no corresponding local setting.
		goal.SettingsVersion = ""
		if len(goal.NodeLabels) == 0 {
			goal.NodeLabels = nil
		}
		if len(goal.NodeTaints) == 0 {
			goal.NodeTaints = nil
		} else {
			goal.NodeTaints = slices.Clone(goal.NodeTaints)
			slices.Sort(goal.NodeTaints)
		}
		return goal
	}
	remote = normalize(remote)
	local = normalize(local)
	return reflect.DeepEqual(remote, local)
}

func machineGoalFailure(
	requireMachineRegistration bool,
	operation string,
	err error,
) (*aksmachine.GoalState, preflight.Result, error) {
	result := preflight.Warning(machineGoalCheckName, "AKS Machine goal", "AKS Machine goal is unavailable; validating the local bootstrap goal")
	if requireMachineRegistration {
		result = preflight.Error(machineGoalCheckName, "AKS Machine goal", "AKS Machine goal is unavailable but machine registration is required")
	}
	return nil, result, fmt.Errorf("%s: %w", operation, err)
}
