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
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/noneops"
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
	store, err := daemon.NewFileStateStore()
	if err != nil {
		return err
	}
	state, err := store.Load(ctx)
	if err != nil {
		return err
	}
	mode := config.DefaultIsolationMode
	orphanedArtifacts := false
	if state != nil {
		mode = state.Isolation
	} else if noneops.Present() {
		mode = isolation.ModeNone
	} else {
		orphanedArtifacts, err = agentruntime.NoneUpgradeArtifactsPresent()
		if err != nil {
			return err
		}
	}
	if orphanedArtifacts {
		// With no applied state or SDK ownership traces, shared host paths may
		// belong to another runtime; remove only known recovery artifacts.
		return phases.ExecuteTask(ctx, logger, phases.Serial(logger,
			agentdeployment.UninstallService(logger),
			daemon.ResetHostHub(logger),
			agentruntime.CleanupNoneUpgradeArtifacts(),
			config.RemoveRuntimeDirs(logger),
		))
	}
	worker, err := agentruntime.New(mode)
	if err != nil {
		return err
	}
	tasks := phases.Serial(logger,
		agentdeployment.UninstallService(logger),
		daemon.ResetWorker(logger, worker, mode),
		config.RemoveRuntimeDirs(logger),
	)
	return phases.ExecuteTask(ctx, logger, tasks)
}
