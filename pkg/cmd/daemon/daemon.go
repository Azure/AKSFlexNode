package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine/local"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/daemon"
	"github.com/Azure/AKSFlexNode/pkg/logger"
)

func NewCommand() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:     "daemon",
		Aliases: []string{"agent"},
		Short:   "Run the AKS Flex Node daemon",
		Long: "Run the long-lived AKS Flex Node daemon with automatic status tracking " +
			"and self-recovery. This command is intended to be launched by systemd after bootstrap.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadConfig(configPath)
			if err != nil {
				return fmt.Errorf("failed to load config from %s: %w", configPath, err)
			}
			logger := logger.CreateLogger(cfg.Agent.LogLevel, cfg.Agent.LogDir)

			if cfg.Agent.E2EMode {
				return runDaemonE2E(cmd.Context(), cfg, logger)
			}
			return runDaemon(cmd.Context(), cfg, logger)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to configuration JSON file (required)")
	_ = cmd.MarkFlagRequired("config")
	return cmd
}

func runDaemon(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	logger.Info("TODO: production agent daemon requires AKS RP machine client implementation")
	<-ctx.Done()
	return ctx.Err()
}

func runDaemonE2E(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	logger.Info("running agent daemon in e2e mode", "machineFile", local.E2EMachineFilePath)
	machines, err := local.NewClient(local.E2EMachineFilePath)
	if err != nil {
		return fmt.Errorf("create local AKS machine client: %w", err)
	}
	return daemon.Run(ctx, cfg, logger, machines)
}
