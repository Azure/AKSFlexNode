package nspawnlifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	flexconfig "github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/daemon"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	sharedlifecycle "github.com/Azure/unbounded/pkg/agent/nspawnlifecycle"
)

const persistedConfigPath = flexconfig.ConfigDir + "/config.json"

type lifecycle interface {
	PreStart(context.Context, string) error
	PostStart(context.Context, string) error
	Reconcile(context.Context, string) error
}

type lifecycleFactory func(*slog.Logger) (lifecycle, error)

// NewCommand returns the application-owned wrapper for Unbounded's nspawn
// lifecycle operations. Shared rootfs provisioning copies this executable to
// the stable host helper path used by its generated systemd units.
func NewCommand() *cobra.Command {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	return newCommand(log, func(log *slog.Logger) (lifecycle, error) {
		return newLifecycle(log, persistedConfigPath)
	}, daemon.MigrateHostRoot)
}

// newCommand builds the phases. Only pre-start migrates the host root: it
// regenerates the machine's config, which names the lifecycle helper under it,
// while post-start and reconcile write nothing there.
func newCommand(log *slog.Logger, factory lifecycleFactory, migrate func(*slog.Logger) error) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "nspawn-lifecycle",
		Short:  "Run internal nspawn lifecycle hooks",
		Hidden: true,
	}

	cmd.AddCommand(
		newPhaseCommand(log, factory, "pre-start", "Refresh host-side nspawn state before machine start", func(ctx context.Context, lifecycle lifecycle, machine string) error {
			return lifecycle.PreStart(ctx, machine)
		}, migrate),
		newPhaseCommand(log, factory, "post-start", "Reconcile in-machine state after machine start", func(ctx context.Context, lifecycle lifecycle, machine string) error {
			return lifecycle.PostStart(ctx, machine)
		}, nil),
		newPhaseCommand(log, factory, "reconcile", "Restart a machine and run its lifecycle reconciliation", func(ctx context.Context, lifecycle lifecycle, machine string) error {
			return lifecycle.Reconcile(ctx, machine)
		}, nil),
	)

	return cmd
}

// newPhaseCommand runs a lifecycle phase, migrating the host root first unless
// migrate is nil. When the host root cannot be migrated, the phase is skipped
// and the machine starts with the config it has, rather than not at all. The
// daemon refuses to start on such a host, which is where the problem is
// reported.
func newPhaseCommand(
	log *slog.Logger,
	factory lifecycleFactory,
	phase, short string,
	run func(context.Context, lifecycle, string) error,
	migrate func(*slog.Logger) error,
) *cobra.Command {
	return &cobra.Command{
		Use:    phase + " MACHINE",
		Short:  short,
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateMachine(args[0]); err != nil {
				return err
			}

			if migrate != nil {
				if err := migrate(log); err != nil {
					log.Warn("not running the nspawn lifecycle phase: the host root cannot be migrated; the machine keeps its config",
						"phase", phase, "machine", args[0], "error", err)

					return nil
				}
			}

			lifecycle, err := factory(log)
			if err != nil {
				return fmt.Errorf("create nspawn lifecycle: %w", err)
			}

			return run(cmd.Context(), lifecycle, args[0])
		},
	}
}

func newLifecycle(log *slog.Logger, configPath string) (*sharedlifecycle.Lifecycle, error) {
	return sharedlifecycle.New(log, sharedlifecycle.Hooks{
		LoadConfig: configLoader(configPath),
	})
}

func configLoader(configPath string) sharedlifecycle.ConfigLoader {
	return func(_ context.Context, machineName string) (*agentconfig.AgentConfig, bool, error) {
		cfg, err := flexconfig.LoadConfig(configPath)
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, fmt.Errorf("load persisted AKS Flex config %s: %w", configPath, err)
		}

		return flexconfig.ToAgentConfig(cfg, machineName), true, nil
	}
}

func validateMachine(machineName string) error {
	if machineName != goalstates.NSpawnMachineKube1 && machineName != goalstates.NSpawnMachineKube2 {
		return fmt.Errorf("unknown nspawn machine %q", machineName)
	}

	return nil
}
