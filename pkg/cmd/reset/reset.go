package reset

import (
	"context"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/Azure/AKSFlexNode/pkg/config"
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
			return runReset(cmd.Context(), log)
		},
	}
}

func runReset(ctx context.Context, logger *slog.Logger) error {
	tasks := phases.Serial(logger,
		agentdeployment.UninstallService(logger),
		(agentruntime.NSpawn{}).Reset(logger),
		config.RemoveRuntimeDirs(logger),
	)
	return phases.ExecuteTask(ctx, logger, tasks)
}
