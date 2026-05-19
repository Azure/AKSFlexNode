package start

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/aksmachine/local"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/daemon"
	"github.com/Azure/AKSFlexNode/pkg/logger"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

func NewCommand() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:     "start",
		Aliases: []string{"bootstrap"},
		Short:   "Bootstrap the node and start the agent service",
		Long:    "Install the systemd unit, bootstrap the nspawn-based AKS worker node, then enable and start the agent daemon through systemd.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadConfig(configPath)
			if err != nil {
				return fmt.Errorf("failed to load config from %s: %w", configPath, err)
			}
			logger := logger.CreateLogger(cfg.Agent.LogLevel, cfg.Agent.LogDir)

			if err := runStart(cmd.Context(), cfg, logger); err != nil {
				return err
			}

			fmt.Println()
			fmt.Println("AKS Flex Node agent service started successfully.")
			fmt.Println()
			fmt.Println("Next steps:")
			fmt.Println("  Check service status: systemctl status aks-flex-node-agent")
			fmt.Println("  View service logs:    journalctl -u aks-flex-node-agent -f")
			fmt.Println("  Stop agent:           systemctl stop aks-flex-node-agent")
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to configuration JSON file (required)")
	_ = cmd.MarkFlagRequired("config")

	return cmd
}

func runStart(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	machines, err := newMachineClient(cfg, logger)
	if err != nil {
		return err
	}
	machine, err := machines.Get(ctx)
	if err != nil {
		return fmt.Errorf("get AKS machine for daemon state seed: %w", err)
	}
	state := daemon.SeededState(machine.Goal)
	machineName := state.ActiveMachine
	stateStore, err := daemon.NewFileStateStore()
	if err != nil {
		return err
	}

	if err := config.BackfillClusterConfigWithUserCredentials(ctx, cfg, logger); err != nil {
		return fmt.Errorf("bootstrap failed at step enrich-cluster-config: %w", err)
	}

	agentCfg := config.ToAgentConfig(cfg, machineName)
	gs, err := goalstates.ResolveMachine(logger, agentCfg, machineName, nil)
	if err != nil {
		return fmt.Errorf("bootstrap failed to resolve goal state: %w", err)
	}

	tasks := phases.Serial(logger,
		daemon.SetupHost(cfg, logger),
		daemon.StartNode(cfg, logger, machineName, gs, stateStore, state),
		daemon.InstallService(logger),
	)
	start := time.Now()
	if err := phases.ExecuteTask(ctx, logger, tasks); err != nil {
		return fmt.Errorf("bootstrap failed: %w", err)
	}
	logger.Info("operation completed successfully", "operation", "bootstrap", "duration", time.Since(start))

	return nil
}

func newMachineClient(cfg *config.Config, logger *slog.Logger) (aksmachine.MachineClient, error) {
	if cfg.Agent.E2EMode {
		logger.Info("using local e2e AKS machine client", "machineFile", local.E2EMachineFilePath)
		machines, err := local.NewClient(local.E2EMachineFilePath)
		if err != nil {
			return nil, fmt.Errorf("create local AKS machine client: %w", err)
		}
		return machines, nil
	}
	logger.Info("TODO: using no-op AKS machine client until AKS RP implementation is available")
	return aksmachine.NewNoopClient(cfg), nil
}
