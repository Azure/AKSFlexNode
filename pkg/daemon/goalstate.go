package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"

	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
)

// ResolveMachineGoalState overlays AKS Machine-owned settings on the local
// host configuration before resolving the nspawn goal.
func ResolveMachineGoalState(
	ctx context.Context,
	log *slog.Logger,
	cfg *config.Config,
	machineName string,
	goal *aksmachine.GoalState,
) (*agentconfig.AgentConfig, *goalstates.MachineGoalState, *goalstates.ContainerImageArchiveStaging, error) {
	effectiveGoal, err := effectiveMachineGoal(cfg, goal)
	if err != nil {
		return nil, nil, nil, err
	}
	resolvedConfig := cfg.DeepCopy()
	if resolvedConfig == nil {
		return nil, nil, nil, fmt.Errorf("copy config for machine goal")
	}
	resolvedConfig.Components.Kubernetes = effectiveGoal.KubernetesVersion
	resolvedConfig.Node.MaxPods = effectiveGoal.MaxPods
	resolvedConfig.Node.Labels = maps.Clone(effectiveGoal.NodeLabels)
	resolvedConfig.Node.Taints = slices.Clone(effectiveGoal.NodeTaints)
	resolvedConfig.Node.Kubelet.ImageGCHighThreshold = effectiveGoal.KubeletConfig.ImageGCHighThreshold
	resolvedConfig.Node.Kubelet.ImageGCLowThreshold = effectiveGoal.KubeletConfig.ImageGCLowThreshold
	return config.ResolveMachineGoalState(ctx, log, resolvedConfig, machineName)
}

func effectiveMachineGoal(cfg *config.Config, goal *aksmachine.GoalState) (*aksmachine.GoalState, error) {
	localGoal, err := aksmachine.GoalStateFromConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build local machine goal: %w", err)
	}
	if goal == nil {
		return &localGoal, nil
	}
	effectiveGoal, err := aksmachine.EffectiveGoal(*goal, localGoal)
	if err != nil {
		return nil, err
	}
	return &effectiveGoal, nil
}

func goalForRestart(cfg *config.Config, state *State) (*aksmachine.GoalState, error) {
	if state != nil && state.AppliedGoal != nil {
		goal := cloneGoalState(*state.AppliedGoal)
		if err := goal.ValidateEffective(); err != nil {
			return nil, fmt.Errorf("validate persisted restart goal: %w", err)
		}
		return goal, nil
	}

	goal, err := aksmachine.GoalStateFromConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build restart goal from config: %w", err)
	}
	if state != nil {
		goal.SettingsVersion = state.AppliedSettingsVersion
		if state.AppliedKubernetesVersion != "" {
			goal.KubernetesVersion = state.AppliedKubernetesVersion
		}
	}
	if err := goal.ValidateEffective(); err != nil {
		return nil, fmt.Errorf("validate legacy restart goal: %w", err)
	}
	return &goal, nil
}
