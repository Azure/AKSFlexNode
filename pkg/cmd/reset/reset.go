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
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/noneops"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

func NewCommand() *cobra.Command {
	var requestedMode string
	cmd := &cobra.Command{
		Use:     "reset",
		Aliases: []string{"unbootstrap"},
		Short:   "Remove AKS node configuration",
		Long:    "Clean up and remove AKS Flex Node components while preserving externally managed Azure Arc state",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := logger.CreateLogger("info", "")
			return runReset(cmd.Context(), log, isolation.Mode(requestedMode))
		},
	}
	cmd.Flags().StringVar(&requestedMode, "isolation", "", "Worker isolation for recovery when applied state is missing or unreadable (nspawn, none)")
	return cmd
}

func runReset(ctx context.Context, logger *slog.Logger, requestedMode isolation.Mode) error {
	store, err := daemon.NewFileStateStore()
	if err != nil {
		return err
	}
	state, loadErr := store.Load(ctx)
	mode, err := selectResetMode(state, loadErr, requestedMode)
	if err != nil {
		return err
	}
	if loadErr != nil {
		logger.Warn("resetting explicitly selected worker without trusting unreadable applied state", "isolation", mode, "error", loadErr)
	}

	orphanedArtifacts := false
	if mode == "" {
		mode = config.DefaultIsolationMode
		if noneops.Present() {
			mode = isolation.ModeNone
		} else {
			orphanedArtifacts, err = agentruntime.NoneUpgradeArtifactsPresent()
			if err != nil {
				return err
			}
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

// selectResetMode ignores any State returned with an error. An empty result
// leaves runtime discovery in control when State is absent, not unreadable.
func selectResetMode(state *daemon.State, loadErr error, requestedMode isolation.Mode) (isolation.Mode, error) {
	if requestedMode != "" {
		if err := config.ValidateIsolation(requestedMode); err != nil {
			return "", fmt.Errorf("invalid reset isolation: %w", err)
		}
	}
	if loadErr != nil {
		if requestedMode == "" {
			return "", fmt.Errorf("cannot read applied worker state; verify the worker isolation and rerun reset --isolation <mode>: %w", loadErr)
		}
		return requestedMode, nil
	}
	if state != nil {
		if requestedMode != "" && requestedMode != state.Isolation {
			return "", fmt.Errorf("reset isolation %q conflicts with applied worker isolation %q", requestedMode, state.Isolation)
		}
		return state.Isolation, nil
	}
	return requestedMode, nil
}
