package daemon

import (
	"context"
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// workerRuntime constructs worker execution tasks. The daemon executes them and
// owns lifecycle decisions, applied State, and remote status publication.
//
// The daemon drives these transitions; the runtime stores no separate phase:
//
//	Bootstrap: Start -> save applied State
//	Restart:   Restart(active slot), without advancing applied State
//	Repave:    Stop(old) -> CleanupNetwork -> ensure agent hooks
//	           -> Start(new) -> save applied State -> Retire(old)
//	Reset:     Reset -> clear daemon configuration and State
//
// An error stops the sequence without implying that earlier actions rolled back.
// In particular, a failed State save must not retire the previous slot. Reset
// does not stop the daemon, allowing AgentReset to publish completion first.
type workerRuntime interface {
	// Start provisions and starts the selected slot, including Flex guest artifacts
	// and NPD. The daemon records it as applied only after this task succeeds.
	Start(*config.Config, *slog.Logger, string, *goalstates.MachineGoalState, *goalstates.ContainerImageArchiveStaging) phases.Task

	// Restart restarts the active slot in place rather than provisioning a
	// replacement or changing which goal the daemon considers applied.
	Restart(*config.Config, *slog.Logger, string, *goalstates.MachineGoalState, *goalstates.ContainerImageArchiveStaging) phases.Task

	// Stop stops the old worker before replacement while retaining its filesystem
	// until the replacement has been started and its State saved.
	Stop(*slog.Logger, string) phases.Task

	// CleanupNetwork removes the stopped worker's host networking so the
	// replacement can configure it without inheriting stale runtime resources.
	CleanupNetwork(*slog.Logger) phases.Task

	// Retire removes the superseded slot after the daemon saves the replacement
	// as applied. It must not run when replacement startup or persistence fails.
	Retire(*slog.Logger, string) phases.Task

	// Reset tears down both worker slots and their owned runtime artifacts.
	// Daemon configuration/State cleanup and agent uninstall are caller-owned.
	Reset(*slog.Logger) phases.Task
}

type machineGoalResolver func(context.Context, *slog.Logger, *config.Config, string) (config.ResolvedMachineGoal, error)
