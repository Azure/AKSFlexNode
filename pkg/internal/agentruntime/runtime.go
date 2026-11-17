// Package agentruntime executes worker operations without owning daemon state.
package agentruntime

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/config"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/noneops"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// Node contains a resolved worker configuration and placement.
type Node struct {
	Slot      string
	Config    *agentconfig.AgentConfig
	GoalState *goalstates.MachineGoalState
}

// Inputs carry execution inputs resolved by the daemon. Previous is set for a repave.
type Inputs struct {
	Config   *config.Config
	Node     Node
	Previous *Node
	Archives *goalstates.ContainerImageArchiveStaging
}

// WorkerContext describes the SDK filesystem and executor for the active worker.
type WorkerContext struct {
	Root        isolation.NodeRoot
	Exec        func(context.Context, ...string) (string, error)
	KubeletUnit string
	Mode        isolation.Mode
}

// Runtime executes worker actions. The daemon owns sequencing and persistence.
type Runtime interface {
	Worker(*slog.Logger, Node) (WorkerContext, error)
	Start(*slog.Logger, Inputs) phases.Task
	Restart(*slog.Logger, Inputs) phases.Task
	// Prepare stops and cleans nspawn before replacement, or recovers a none upgrade.
	Prepare(*slog.Logger, Inputs) phases.Task
	// Retire runs after the daemon saves applied state.
	Retire(*slog.Logger, Inputs) phases.Task
	Reset(*slog.Logger) phases.Task
}

// New selects an already validated worker mode.
func New(mode isolation.Mode) (Runtime, error) {
	switch mode {
	case isolation.ModeNSpawn:
		return NSpawn{}, nil
	case isolation.ModeNone:
		return noneRuntime{newNode: noneops.New}, nil
	default:
		return nil, fmt.Errorf("unsupported worker isolation %q", mode)
	}
}
