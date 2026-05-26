package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
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

			return runDaemon(cmd.Context(), cfg, logger)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to configuration JSON file (required)")
	_ = cmd.MarkFlagRequired("config")
	return cmd
}

func runDaemon(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	machines, err := aksmachine.NewMachineClient(cfg, logger)
	if err != nil {
		return fmt.Errorf("create AKS machine client: %w", err)
	}

	return daemon.Run(ctx, cfg, logger, machines)
}
