package reset

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/daemon"
	"github.com/Azure/AKSFlexNode/pkg/internal/agentdeployment"
	"github.com/Azure/AKSFlexNode/pkg/internal/agentruntime"
	"github.com/Azure/AKSFlexNode/pkg/logger"
	"github.com/Azure/unbounded/pkg/agent/containerops"
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
	mode := isolation.ModeNSpawn
	if state != nil {
		mode = state.Isolation
	} else {
		nonePresent, ociPresent := noneops.Present(), containerops.Present()
		if nonePresent && ociPresent {
			return fmt.Errorf("both host-native and OCI worker traces exist; inspect before reset")
		}
		switch {
		case nonePresent:
			mode = isolation.ModeNone
		case ociPresent:
			mode = isolation.ModeContainer
		default:
			noneArtifacts, err := agentruntime.NoneUpgradeArtifactsPresent()
			if err != nil {
				return err
			}
			ociArtifact, err := agentruntime.ContainerUpgradeArtifactPresent()
			if err != nil {
				return err
			}
			if noneArtifacts || ociArtifact {
				// Without ownership traces, shared paths may belong to another
				// runtime. Remove only the known recovery files.
				tasks := []phases.Task{agentdeployment.UninstallService(logger)}
				if noneArtifacts {
					tasks = append(tasks, daemon.ResetHostHub(logger), agentruntime.CleanupNoneUpgradeArtifacts())
				}
				if ociArtifact {
					tasks = append(tasks, agentruntime.CleanupContainerUpgradeArtifact())
				}
				tasks = append(tasks, config.RemoveRuntimeDirs(logger))
				return phases.ExecuteTask(ctx, logger, phases.Serial(logger, tasks...))
			}
		}
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
