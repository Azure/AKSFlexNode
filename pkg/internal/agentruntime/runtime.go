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

// Runtime constructs worker execution tasks. The daemon owns lifecycle decisions,
// applied State, and remote status publication; the SDK owns runtime recovery.
//
// The daemon drives these transitions:
//
//	Bootstrap: Start -> save applied State
//	Reapply:   Prepare(applied) -> Restart, without advancing applied State
//	Repave:    Prepare(previous) -> agent hooks or host Hub cleanup
//	           -> Start(target) -> save applied State -> Retire(previous)
//	Reset:     stop workers -> host Hub cleanup if needed -> teardown -> clear configuration/State
//
// An error stops the sequence without implying that earlier actions rolled back.
// Start must succeed before persistence, and persistence must succeed before
// Retire. Recovery guarantees are backend-specific; this interface adds no journal.
type Runtime interface {
	// Worker resolves the selected worker's filesystem, executor, and service
	// context for components without loading or updating daemon State.
	Worker(*slog.Logger, Node) (WorkerContext, error)

	// Start provisions the first worker or applies a replacement when Previous
	// is supplied, then starts the worker and converges its NPD service.
	Start(*slog.Logger, Inputs) phases.Task

	// Restart restarts the installed worker in place without provisioning a
	// replacement, replacing kubelet identity, or advancing applied State.
	Restart(*slog.Logger, Inputs) phases.Task

	// Prepare stops and cleans nspawn before replacement. For none it runs SDK
	// recovery against the applied config, using Previous when replacing a worker.
	Prepare(*slog.Logger, Inputs) phases.Task

	// Retire removes the superseded slot or commits/prunes a none SDK upgrade.
	// The daemon must successfully save the target as applied before invoking it.
	Retire(*slog.Logger, Inputs) phases.Task

	// PlanReset discovers owned worker runtimes at execution time, independently
	// of applied State. Callers stop workers before component cleanup and Finish;
	// State deletion, status publication and uninstall stay outside the plan.
	PlanReset(*slog.Logger) ResetPlan
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
