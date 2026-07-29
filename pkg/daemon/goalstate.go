package daemon

import (
	"context"
	"log/slog"
	"maps"
	"slices"

	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
)

// ResolveMachineGoalState resolves an nspawn goal. This final nspawn goal remains a hybrid:
// only Kubernetes version, labels, and taints are overlaid from GoalState;
// networking, credentials, runtime versions, images, mounts, etc. still come from config.
func ResolveMachineGoalState(
	ctx context.Context,
	log *slog.Logger,
	cfg *config.Config,
	machineName string,
	goal *aksmachine.GoalState,
) (*agentconfig.AgentConfig, *goalstates.MachineGoalState, *goalstates.ContainerImageArchiveStaging, error) {
	resolvedConfig := cfg.DeepCopy()
	if goal != nil {
		resolvedConfig.Components.Kubernetes = goal.KubernetesVersion
		resolvedConfig.Node.Labels = maps.Clone(goal.NodeLabels)
		resolvedConfig.Node.Taints = slices.Clone(goal.NodeTaints)
	}
	return config.ResolveMachineGoalState(ctx, log, resolvedConfig, machineName)
}
