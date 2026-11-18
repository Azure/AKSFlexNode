package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/internal/agentdeployment"
	"github.com/Azure/AKSFlexNode/pkg/internal/agentruntime"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func prepareDaemonStartup(ctx context.Context, cfg *config.Config, log *slog.Logger, store stateStore, containerized bool) (agentruntime.Runtime, func(context.Context) error, error) {
	applied, err := store.Load(ctx)
	if err != nil {
		return nil, nil, err
	}
	if err := validateStartupState(cfg.Bootstrap.Isolation, applied); err != nil {
		return nil, nil, err
	}
	worker, err := agentruntime.New(cfg.Bootstrap.Isolation)
	if err != nil {
		return nil, nil, err
	}

	convergeHeldAssets, err := prepareHostStartup(ctx, cfg, log, containerized)
	if err != nil {
		return nil, nil, err
	}
	if err := recoverStartupWorker(ctx, cfg, log, worker, applied); err != nil {
		return nil, nil, err
	}

	// Converge container assets before building clients, even if credentials fail.
	if containerized {
		convergence := agentdeployment.NewContainerConvergence(log)
		held, err := convergeContainerStartup(ctx, store, convergence)
		if err != nil {
			return nil, nil, err
		}
		if held {
			convergeHeldAssets = convergence.WriteUnits
		}
	}
	return worker, convergeHeldAssets, nil
}

func validateStartupState(mode isolation.Mode, applied *State) error {
	if applied != nil && applied.Isolation != mode {
		return fmt.Errorf("worker isolation change requires reset")
	}
	switch mode {
	case isolation.ModeNSpawn:
		return nil
	case isolation.ModeNone, isolation.ModeContainer:
		if applied == nil {
			return fmt.Errorf("applied %s worker state is missing", mode)
		}
		return nil
	default:
		return fmt.Errorf("unsupported worker isolation %q", mode)
	}
}

func prepareHostStartup(ctx context.Context, cfg *config.Config, log *slog.Logger, containerized bool) (func(context.Context) error, error) {
	if !containerized {
		if err := agentdeployment.MigrateHostRoot(log); err != nil {
			return nil, err
		}
		if err := agentdeployment.InstallHostBinary(ctx, log); err != nil {
			return nil, err
		}
	}

	// Keep the last-good assets while an AgentUpgrade is pending.
	convergeHeldAssets, err := ensureHostInstall(ctx, log, cfg, containerized)
	if err != nil {
		return nil, err
	}
	// Remove the old seed only after converging the units that could name it.
	if !containerized {
		if err := agentdeployment.RemoveDaemonLegacySeed(log); err != nil {
			log.Warn("failed to remove the agent binary left in /usr/local/bin", "error", err)
		}
	}
	return convergeHeldAssets, nil
}

func recoverStartupWorker(ctx context.Context, cfg *config.Config, log *slog.Logger, worker agentruntime.Runtime, applied *State) error {
	if cfg.Bootstrap.Isolation == isolation.ModeNSpawn {
		return nil
	}
	input := agentruntime.Inputs{
		Config: cfg,
		Node: agentruntime.Node{
			Slot:   applied.ActiveMachine,
			Config: applied.NodeConfig,
		},
	}
	if err := worker.Recover(log, input).Do(ctx); err != nil {
		return fmt.Errorf("recover worker at startup: %w", err)
	}
	return nil
}

type containerStartupConvergence interface {
	Converge(context.Context) (bool, error)
	SyncActiveWorker(string) error
}

func convergeContainerStartup(ctx context.Context, store stateStore, convergence containerStartupConvergence) (bool, error) {
	held, err := convergence.Converge(ctx)
	if err != nil {
		return held, err
	}

	state, err := store.Load(ctx)
	if err != nil {
		return held, fmt.Errorf("load daemon state: %w", err)
	}
	if state != nil && validActiveMachine(state.ActiveMachine) {
		if err := convergence.SyncActiveWorker(state.ActiveMachine); err != nil {
			return held, err
		}
	}
	return held, nil
}

// openGateAfterAgentUpgradeJudgment publishes the result of an AgentUpgrade the daemon started
// for, retrying, and then runs proceed, which opens the startup gate, unless the daemon has to be
// replaced first, as after it queued its restart from the host root. It runs nothing while the
// upgrade's signal remains, as when recovery scheduled a restart into the last-good binary,
// which opens the gate instead. Before proceed, it converges the service assets the daemon held
// back at startup, now that the upgrade is judged; failing that is only logged, as the
// last-good version's assets still work, and the next start converges them again.
func openGateAfterAgentUpgradeJudgment(
	ctx context.Context,
	log *slog.Logger,
	publish func(context.Context) error,
	readSignal func() (*agentdeployment.UpgradeSignal, error),
	convergeHeldAssets func(context.Context) error,
	proceed func(),
	retry time.Duration,
) {
	for {
		publishErr := publish(ctx)
		pending, readErr := readSignal()
		switch {
		case publishErr != nil:
			log.Warn("failed to publish AgentUpgrade startup result; will retry", "error", publishErr)
		case readErr != nil:
			log.Warn("failed to confirm AgentUpgrade startup signal; will retry", "error", readErr)
		case pending != nil:
			// Recovery scheduled a restart. Keep reconciliation gated until the
			// last-good process starts and consumes the retained signal.
			return
		default:
			if err := convergeHeldAssets(ctx); err != nil {
				log.Warn("failed to update the agent's service assets after its upgrade; the last-good version's remain",
					"error", err)
			}
			proceed()
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
	}
}

// ensureHostInstall converges the agent's binary layout and units on the host, as
// agentdeployment.EnsureDaemonServiceAssets does, and returns what writes those it held back while an
// AgentUpgrade was pending, for the daemon to run once the upgrade is judged. In its container
// the agent has no binary layout, the image carries it, and container convergence handles
// its units, so it does nothing there.
func ensureHostInstall(
	ctx context.Context,
	log *slog.Logger,
	cfg *config.Config,
	containerized bool,
) (func(context.Context) error, error) {
	if containerized {
		return noHeldAssets, nil
	}
	held, err := agentdeployment.EnsureDaemonServiceAssets(ctx, log, cfg)
	if err != nil || !held {
		return noHeldAssets, err
	}
	return func(ctx context.Context) error {
		_, err := agentdeployment.EnsureDaemonServiceAssets(ctx, log, cfg)
		return err
	}, nil
}

// noHeldAssets is what the daemon runs once an upgrade is judged when it held nothing back.
func noHeldAssets(context.Context) error { return nil }
