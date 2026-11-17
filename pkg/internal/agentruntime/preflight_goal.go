package agentruntime

import (
	"log/slog"

	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/containerops"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/noneops"
)

// PreflightGoalState supplies the SDK root and executor used by the selected worker.
func PreflightGoalState(log *slog.Logger, mode isolation.Mode, cfg *agentconfig.AgentConfig, gs *goalstates.MachineGoalState) (*goalstates.MachineGoalState, error) {
	switch mode {
	case isolation.ModeNone:
		return noneops.New(noneops.Params{Log: log, Config: cfg, GoalState: gs}).GoalState()
	case isolation.ModeContainer:
		return containerops.New(containerops.Params{Log: log, Config: cfg, Slot: gs.NodeStart.MachineName, GoalState: gs}).GoalState()
	default:
		return gs, nil
	}
}
