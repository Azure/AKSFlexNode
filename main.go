package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"go.goms.io/aks/AKSFlexNode/pkg/actions/cmd/apply"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/logger"
)

var (
	configPath string
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "aks-flex-node",
		Short: "AKS Flex Node Agent",
		Long:  "Azure Kubernetes Service Flex Node Agent for edge computing scenarios",
	}

	// Add global flags for configuration
	rootCmd.PersistentFlags().StringVar(&configPath, "config", "", "Path to configuration JSON file (required)")
	// Don't mark as required globally - we'll check in PersistentPreRunE for commands that need it

	// Add commands
	rootCmd.AddCommand(NewAgentCommand())
	// rootCmd.AddCommand(NewApplyCommand())
	rootCmd.AddCommand(NewUnbootstrapCommand())
	rootCmd.AddCommand(NewVersionCommand())
	rootCmd.AddCommand(apply.Command)

	// Set up context with signal handling
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Set up persistent pre-run to initialize config and logger
	rootCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		// Skip config loading for version command
		if cmd.Name() == "version" {
			return nil
		}
		if cmd.Name() == "apply" {
			return nil
		}

		// For other commands, config is required
		if configPath == "" {
			return fmt.Errorf("config path is required for %s command", cmd.Name())
		}

		// Load config if specified
		cfg, err := config.LoadConfig(configPath)
		if err != nil {
			return fmt.Errorf("failed to load config from %s: %w", configPath, err)
		}

		// Setup logger and update context
		ctx := logger.SetupLogger(cmd.Context(), cfg.Agent.LogLevel, cfg.Agent.LogDir)
		cmd.SetContext(ctx)
		return nil
	}

	// Execute command with context
	if err := rootCmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Command execution failed: %v\n", err)
		os.Exit(1)
	}
}
