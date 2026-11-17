package daemon

import (
	"context"
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/config"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// workerRuntime is the worker execution boundary; state and sequencing remain here.
type workerRuntime interface {
	Start(*config.Config, *slog.Logger, string, *goalstates.MachineGoalState, *goalstates.ContainerImageArchiveStaging) phases.Task
	Restart(*config.Config, *slog.Logger, string, *goalstates.MachineGoalState, *goalstates.ContainerImageArchiveStaging) phases.Task
	Stop(*slog.Logger, string) phases.Task
	CleanupNetwork(*slog.Logger) phases.Task
	Retire(*slog.Logger, string) phases.Task
	Reset(*slog.Logger) phases.Task
}

type machineGoalResolver func(context.Context, *slog.Logger, *config.Config, string) (*agentconfig.AgentConfig, *goalstates.MachineGoalState, *goalstates.ContainerImageArchiveStaging, error)
