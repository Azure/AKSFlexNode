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

// ResolveMachineGoalState overlays a complete effective goal on the local host
// configuration before resolving the nspawn goal.
func ResolveMachineGoalState(
	ctx context.Context,
	log *slog.Logger,
	cfg *config.Config,
	machineName string,
	goal aksmachine.GoalState,
) (*agentconfig.AgentConfig, *goalstates.MachineGoalState, *goalstates.ContainerImageArchiveStaging, error) {
	if err := goal.Validate(); err != nil {
		return nil, nil, nil, fmt.Errorf("validate machine goal: %w", err)
	}
	resolvedConfig := cfg.DeepCopy()
	if resolvedConfig == nil {
		return nil, nil, nil, fmt.Errorf("copy config for machine goal")
	}
	resolvedConfig.Components.Kubernetes = goal.KubernetesVersion
	resolvedConfig.Node.MaxPods = goal.MaxPods
	resolvedConfig.Node.Labels = maps.Clone(goal.NodeLabels)
	resolvedConfig.Node.Taints = slices.Clone(goal.NodeTaints)
	resolvedConfig.Node.Kubelet.ImageGCHighThreshold = goal.KubeletConfig.ImageGCHighThreshold
	resolvedConfig.Node.Kubelet.ImageGCLowThreshold = goal.KubeletConfig.ImageGCLowThreshold
	return config.ResolveMachineGoalState(ctx, log, resolvedConfig, machineName)
}

func goalForRestart(cfg *config.Config, state *State) (*aksmachine.GoalState, error) {
	if state != nil && state.AppliedGoal != nil {
		goal := state.AppliedGoal.DeepCopy()
		if err := goal.Validate(); err != nil {
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
	if err := goal.Validate(); err != nil {
		return nil, fmt.Errorf("validate legacy restart goal: %w", err)
	}
	return &goal, nil
}
