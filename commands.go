package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"go.goms.io/aks/AKSFlexNode/pkg/bootstrapper"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/logger"
	"go.goms.io/aks/AKSFlexNode/pkg/status"
)

// Version information variables (set at build time)
var (
	Version   = "dev"
	GitCommit = "unknown"
	BuildTime = "unknown"
)

// NewAgentCommand creates a new agent command
func NewAgentCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Start AKS node agent with Arc connection",
		Long:  "Initialize and run the AKS node agent daemon with automatic status tracking and self-recovery",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAgent(cmd.Context())
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

// runAgent executes the bootstrap process and then runs as daemon
func runAgent(ctx context.Context) error {
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

	// Handle and log the bootstrap result
	if err := handleExecutionResult(result, "bootstrap", logger); err != nil {
		return err
	}

	// After successful bootstrap, transition to daemon mode
	logger.Info("Bootstrap completed successfully, transitioning to daemon mode...")
	return runDaemonLoop(ctx, cfg)
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

// runDaemonLoop runs the periodic status collection and bootstrap monitoring daemon
func runDaemonLoop(ctx context.Context, cfg *config.Config) error {
	logger := logger.GetLoggerFromContext(ctx)
	// Create status file directory - using runtime directory for service or temp for development
	statusFilePath := status.GetStatusFilePath()
	statusDir := filepath.Dir(statusFilePath)
	if err := os.MkdirAll(statusDir, 0750); err != nil {
		return fmt.Errorf("failed to create status directory %s: %w", statusDir, err)
	}

	// Clean up any stale status file on daemon startup
	if _, err := os.Stat(statusFilePath); err == nil {
		logger.Info("Removing stale status file from previous daemon session...")
		if err := os.Remove(statusFilePath); err != nil {
			logger.Warnf("Failed to remove stale status file: %v", err)
		} else {
			logger.Info("Stale status file removed successfully")
		}
	}

	logger.Info("Starting periodic status collection daemon (status: 1 minutes, bootstrap check: 2 minute)")

	// Create tickers for different intervals
	statusTicker := time.NewTicker(1 * time.Minute)
	bootstrapTicker := time.NewTicker(2 * time.Minute)
	defer statusTicker.Stop()
	defer bootstrapTicker.Stop()

	// Collect status immediately on start
	if err := collectAndWriteStatus(ctx, cfg, statusFilePath); err != nil {
		logger.Errorf("Failed to collect initial status: %v", err)
	}

	// Run the periodic collection and monitoring loop
	for {
		select {
		case <-ctx.Done():
			logger.Info("Daemon shutting down due to context cancellation")
			return ctx.Err()
		case <-statusTicker.C:
			logger.Infof("Starting periodic status collection at %s...", time.Now().Format("2006-01-02 15:04:05"))
			if err := collectAndWriteStatus(ctx, cfg, statusFilePath); err != nil {
				logger.Errorf("Failed to collect status at %s: %v", time.Now().Format("2006-01-02 15:04:05"), err)
				// Continue running even if status collection fails
			} else {
				logger.Infof("Status collection completed successfully at %s", time.Now().Format("2006-01-02 15:04:05"))
			}
		case <-bootstrapTicker.C:
			logger.Infof("Starting bootstrap health check at %s...", time.Now().Format("2006-01-02 15:04:05"))
			if err := checkAndBootstrap(ctx, cfg); err != nil {
				logger.Errorf("Auto-bootstrap check failed at %s: %v", time.Now().Format("2006-01-02 15:04:05"), err)
				// Continue running even if bootstrap check fails
			} else {
				logger.Infof("Bootstrap health check completed at %s", time.Now().Format("2006-01-02 15:04:05"))
			}
		}
	}
}

// checkAndBootstrap checks if the node needs re-bootstrapping and performs it if necessary
func checkAndBootstrap(ctx context.Context, cfg *config.Config) error {
	logger := logger.GetLoggerFromContext(ctx)
	// Create status collector to check bootstrap requirements
	collector := status.NewCollector(cfg, logger, Version)

	// Check if bootstrap is needed
	needsBootstrap := collector.NeedsBootstrap(ctx)
	if !needsBootstrap {
		return nil // All good, no action needed
	}

	logger.Info("Node requires re-bootstrapping, initiating auto-bootstrap...")

	// Perform bootstrap
	bootstrapExecutor := bootstrapper.New(cfg, logger)
	result, err := bootstrapExecutor.Bootstrap(ctx)
	if err != nil {
		// Bootstrap failed - remove status file so next check will detect the problem
		removeStatusFile(ctx)
		return fmt.Errorf("auto-bootstrap failed: %s", err)
	}

	// Handle and log the bootstrap result
	if err := handleExecutionResult(result, "auto-bootstrap", logger); err != nil {
		// Bootstrap execution failed - remove status file so next check will detect the problem
		removeStatusFile(ctx)
		return fmt.Errorf("auto-bootstrap execution failed: %s", err)
	}

	logger.Info("Auto-bootstrap completed successfully")
	return nil
}

func removeStatusFile(ctx context.Context) {
	logger := logger.GetLoggerFromContext(ctx)
	statusFilePath := status.GetStatusFilePath()
	if removeErr := os.Remove(statusFilePath); removeErr != nil {
		logger.Debugf("Failed to remove status file: %s", removeErr)
	} else {
		logger.Debug("Removed status file successfully")
	}
}

// collectAndWriteStatus collects current node status and writes it to the status file
func collectAndWriteStatus(ctx context.Context, cfg *config.Config, statusFilePath string) error {
	logger := logger.GetLoggerFromContext(ctx)

	// Create status collector
	collector := status.NewCollector(cfg, logger, Version)

	// Collect comprehensive status
	nodeStatus, err := collector.CollectStatus(ctx)
	if err != nil {
		return fmt.Errorf("failed to collect node status: %w", err)
	}

	// Write status to JSON file
	statusData, err := json.MarshalIndent(nodeStatus, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal status to JSON: %w", err)
	}

	// Write to temporary file first, then rename (atomic operation)
	tempFile := statusFilePath + ".tmp"
	if err := os.WriteFile(tempFile, statusData, 0600); err != nil {
		return fmt.Errorf("failed to write status to temp file: %w", err)
	}

	if err := os.Rename(tempFile, statusFilePath); err != nil {
		return fmt.Errorf("failed to rename temp status file: %w", err)
	}

	logger.Debugf("Status written to %s", statusFilePath)
	return nil
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
