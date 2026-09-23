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
		Short:   "Remove AKS node configuration",
		Long:    "Clean up and remove AKS Flex Node components while preserving externally managed Azure Arc state",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := logger.CreateLogger("info", "")
			return runReset(cmd.Context(), log)
		},
	}
}

func runReset(ctx context.Context, logger *slog.Logger) error {
	// Read before anything is removed: reset deletes the config that records
	// the prefix, and the prefixed files cannot be found without it.
	prefix := daemon.InstalledHostPrefix()

	tasks := phases.Serial(logger,
		daemon.UninstallService(logger, prefix),
		daemon.ResetNode(logger, prefix),
	)
	return phases.ExecuteTask(ctx, logger, tasks)
}
