package main

import (
	"context"
	"fmt"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"go.goms.io/aks/AKSFlexNode/pkg/bootstrapper"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/logger"
)

// Version information variables (set at build time)
var (
	Version   = "dev"
	GitCommit = "unknown"
	BuildTime = "unknown"
)

// NewBootstrapCommand creates a new bootstrap command
func NewBootstrapCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Bootstrap AKS node with Arc connection",
		Long:  "Initialize and configure this machine as an AKS node connected through Azure Arc",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBootstrap(cmd.Context())
		},
	}

	return cmd
}

// NewUnbootstrapCommand creates a new unbootstrap command
func NewUnbootstrapCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "unbootstrap",
		Short: "Remove AKS node configuration and Arc connection",
		Long:  "Clean up and remove all AKS node components and Arc registration from this machine",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUnbootstrap(cmd.Context())
		},
	}

	return cmd
}

// NewVersionCommand creates a new version command
func NewVersionCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Show version information",
		Long:  "Display version, build commit, and build time information",
		Run: func(cmd *cobra.Command, args []string) {
			runVersion()
		},
	}

	return cmd
}

// runBootstrap executes the bootstrap process
func runBootstrap(ctx context.Context) error {
	logger := logger.GetLoggerFromContext(ctx)

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("failed to load config from %s: %w", configPath, err)
	}

	bootstrapExecutor := bootstrapper.New(cfg, logger)
	result, err := bootstrapExecutor.Bootstrap(ctx)
	if err != nil {
		return err
	}

	// Handle and log the result
	return handleExecutionResult(result, "bootstrap", logger)
}

// runUnbootstrap executes the unbootstrap process
func runUnbootstrap(ctx context.Context) error {
	logger := logger.GetLoggerFromContext(ctx)

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("failed to load config from %s: %w", configPath, err)
	}

	bootstrapExecutor := bootstrapper.New(cfg, logger)
	result, err := bootstrapExecutor.Unbootstrap(ctx)
	if err != nil {
		return err
	}

	// Handle and log the result (unbootstrap is more lenient with failures)
	return handleExecutionResult(result, "unbootstrap", logger)
}

// runVersion displays version information
func runVersion() {
	fmt.Printf("AKS Flex Node Agent\n")
	fmt.Printf("Version: %s\n", Version)
	fmt.Printf("Git Commit: %s\n", GitCommit)
	fmt.Printf("Build Time: %s\n", BuildTime)
}

// handleExecutionResult processes and logs execution results
func handleExecutionResult(result *bootstrapper.ExecutionResult, operation string, logger *logrus.Logger) error {
	if result == nil {
		return fmt.Errorf("%s result is nil", operation)
	}

	if result.Success {
		logger.Infof("%s completed successfully (duration: %v, steps: %d)",
			operation, result.Duration, result.StepCount)
		return nil
	}

	if operation == "unbootstrap" {
		// For unbootstrap, log warnings but don't fail completely
		logger.Warnf("%s completed with some failures: %s (duration: %v)",
			operation, result.Error, result.Duration)
		return nil
	}

	// For bootstrap, return error on failure
	return fmt.Errorf("%s failed: %s", operation, result.Error)
}
