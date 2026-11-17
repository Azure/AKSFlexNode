package reset

import (
	"context"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/daemon"
	"github.com/Azure/AKSFlexNode/pkg/internal/agentdeployment"
	"github.com/Azure/AKSFlexNode/pkg/internal/agentruntime"
	"github.com/Azure/AKSFlexNode/pkg/logger"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

func NewCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "reset",
		Aliases: []string{"unbootstrap"},
		Short:   "Remove AKS node configuration",
		Long:    "Clean up and remove AKS Flex Node components while preserving externally managed Azure Arc state",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := logger.CreateLogger("info", "")
			return runReset(cmd.Context(), log, agentdeployment.UninstallService(log), agentruntime.NSpawn{}, config.RemoveRuntimeDirs(log))
		},
	}
}

func runReset(ctx context.Context, log *slog.Logger, uninstall phases.Task, worker agentruntime.Runtime, clearConfig phases.Task) error {
	return phases.ExecuteTask(ctx, log, phases.Serial(log,
		uninstall,
		daemon.ResetWorker(log, worker),
		clearConfig,
	))
}
