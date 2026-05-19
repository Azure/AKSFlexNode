package reset

import (
	"context"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/Azure/AKSFlexNode/pkg/daemon"
	"github.com/Azure/AKSFlexNode/pkg/logger"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

func NewCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "reset",
		Aliases: []string{"unbootstrap"},
		Short:   "Remove AKS node configuration and Arc connection",
		Long:    "Clean up and remove all AKS node components and Arc registration from this machine",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := logger.CreateLogger("info", "")
			return runReset(cmd.Context(), log)
		},
	}
}

func runReset(ctx context.Context, logger *slog.Logger) error {
	tasks := phases.Serial(logger,
		daemon.UninstallService(logger),
		daemon.ResetNode(logger),
	)
	return phases.ExecuteTask(ctx, logger, tasks)
}
