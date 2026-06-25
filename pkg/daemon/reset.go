package daemon

import (
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/arc"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestop"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
)

func ResetNode(log *slog.Logger) phases.Task {
	return phases.Serial(log,
		phases.Parallel(log,
			nodestop.StopNode(log, goalstates.NSpawnMachineKube1),
			nodestop.StopNode(log, goalstates.NSpawnMachineKube2),
		),
		phases.Parallel(log,
			reset.CleanupMachine(log, goalstates.NSpawnMachineKube1),
			reset.CleanupMachine(log, goalstates.NSpawnMachineKube2),
		),
		phases.Parallel(log,
			reset.RemoveNetworkInterfaces(log),
			reset.RemoveWireGuardKeys(log),
		),
		reset.CleanupRoutes(log),
		reset.ReloadSystemd(log),
		config.RemoveRuntimeDirs(log),
		arc.UninstallArc(log),
	)
}
