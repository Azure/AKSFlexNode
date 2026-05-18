package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/aksmachine/local"
	"github.com/Azure/AKSFlexNode/pkg/bootstrapper"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/daemon"
	"github.com/Azure/AKSFlexNode/pkg/logger"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func NewBootstrapCommand() *cobra.Command {
	var configPath string
	var azureConfigSource string
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Bootstrap the node and start the agent service",
		Long:  "Install the systemd unit, bootstrap the nspawn-based AKS worker node, then enable and start the agent daemon through systemd.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, logger, err := initConfigAndLogger(configPath)
			if err != nil {
				return err
			}
			return runBootstrap(cmd.Context(), cfg, logger, azureConfigSource)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to configuration JSON file (required)")
	cmd.Flags().StringVar(&azureConfigSource, "azure-config-source", "", "Source Azure CLI config directory containing auth files")
	_ = cmd.MarkFlagRequired("config")

	return cmd
}

func NewUnbootstrapCommand() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "unbootstrap",
		Short: "Remove AKS node configuration and Arc connection",
		Long:  "Clean up and remove all AKS node components and Arc registration from this machine",
		RunE: func(cmd *cobra.Command, args []string) error {
			l := logger.CreateLogger("info", "")
			if err := daemon.UninstallService(cmd.Context(), l); err != nil {
				return err
			}

			cfg, logger, err := initConfigAndLogger(configPath)
			if err != nil {
				return err
			}
			return runUnbootstrap(cmd.Context(), cfg, logger)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to configuration JSON file (required)")
	_ = cmd.MarkFlagRequired("config")
	return cmd
}

func initConfigAndLogger(configPath string) (*config.Config, *slog.Logger, error) {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load config from %s: %w", configPath, err)
	}

	l := logger.CreateLogger(cfg.Agent.LogLevel, cfg.Agent.LogDir)
	return cfg, l, nil
}

func runBootstrap(ctx context.Context, cfg *config.Config, logger *slog.Logger, azureConfigSource string) error {
	if err := daemon.InstallService(ctx, logger, azureConfigSource); err != nil {
		return err
	}

	if err := config.EnsureRuntimeDir(); err != nil {
		return err
	}

	machineName := goalstates.NSpawnMachineKube1

	machines, err := newMachineClient(cfg, logger)
	if err != nil {
		return err
	}
	bootstrapExecutor := bootstrapper.New(cfg, logger, machineName, machines)
	result, err := bootstrapExecutor.Bootstrap(ctx)
	if err != nil {
		return err
	}

	if err := handleExecutionResult(result, "bootstrap", logger); err != nil {
		return err
	}
	printBootstrapNextSteps()
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

func printBootstrapNextSteps() {
	fmt.Println()
	fmt.Println("AKS Flex Node agent service started successfully.")
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Println("  Check service status: systemctl status aks-flex-node-agent")
	fmt.Println("  View service logs:    journalctl -u aks-flex-node-agent -f")
	fmt.Println("  Stop agent:           systemctl stop aks-flex-node-agent")
}

func runUnbootstrap(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	if err := config.EnsureRuntimeDir(); err != nil {
		return err
	}

	machineName, err := daemon.ActiveMachineFromState(ctx)
	if err != nil {
		return fmt.Errorf("resolve active machine from daemon state: %w", err)
	}

	bootstrapExecutor := bootstrapper.New(cfg, logger, machineName, nil)
	result, err := bootstrapExecutor.Unbootstrap(ctx)
	if err != nil {
		return err
	}

	return handleExecutionResult(result, "unbootstrap", logger)
}

func handleExecutionResult(result *bootstrapper.ExecutionResult, operation string, logger *slog.Logger) error {
	if result == nil {
		return fmt.Errorf("%s result is nil", operation)
	}

	if result.Success {
		logger.Info("operation completed successfully", "operation", operation, "duration", result.Duration, "steps", result.StepCount)
		return nil
	}

	if operation == "unbootstrap" {
		logger.Warn("operation completed with some failures", "operation", operation, "error", result.Error, "duration", result.Duration)
		return nil
	}

	return fmt.Errorf("%s failed: %s", operation, result.Error)
}
