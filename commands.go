package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/AKSFlexNode/pkg/bootstrapper"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/daemon"
	"github.com/Azure/AKSFlexNode/pkg/drift"
	"github.com/Azure/AKSFlexNode/pkg/logger"
	"github.com/Azure/AKSFlexNode/pkg/spec"
	"github.com/Azure/AKSFlexNode/pkg/status"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

// Version information variables (set at build time)
var (
	Version   = "dev"
	GitCommit = "unknown"
	BuildTime = "unknown"
)

func NewAgentCommand() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Run the AKS Flex Node daemon",
		Long:  "Run the long-lived AKS Flex Node daemon with automatic status tracking and self-recovery. This command is intended to be launched by systemd after bootstrap.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, logger, err := initConfigAndLogger(configPath)
			if err != nil {
				return err
			}
			return runAgentDaemon(cmd.Context(), cfg, logger)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to configuration JSON file (required)")
	_ = cmd.MarkFlagRequired("config")
	return cmd
}

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

func NewVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show version information",
		Long:  "Display version, build commit, and build time information",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("AKS Flex Node Agent\n")
			fmt.Printf("Version: %s\n", Version)
			fmt.Printf("Git Commit: %s\n", GitCommit)
			fmt.Printf("Build Time: %s\n", BuildTime)
		},
	}
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

	if err := spec.EnsureRuntimeDir(); err != nil {
		return err
	}

	machineName := goalstates.NSpawnMachineKube1

	bootstrapExecutor := bootstrapper.New(cfg, logger, machineName)
	result, err := bootstrapExecutor.Bootstrap(ctx)
	if err != nil {
		return err
	}

	if err := handleExecutionResult(result, "bootstrap", logger); err != nil {
		return err
	}

	logger.Info("Bootstrap completed successfully, starting agent systemd service...")
	return daemon.EnableAndStartService(ctx, logger)
}

func runAgentDaemon(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	if err := spec.EnsureRuntimeDir(); err != nil {
		return err
	}

	machineName := goalstates.NSpawnMachineKube1
	return runDaemonLoop(ctx, cfg, logger, machineName)
}

func runUnbootstrap(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	if err := spec.EnsureRuntimeDir(); err != nil {
		return err
	}

	machineName := goalstates.NSpawnMachineKube1

	bootstrapExecutor := bootstrapper.New(cfg, logger, machineName)
	result, err := bootstrapExecutor.Unbootstrap(ctx)
	if err != nil {
		return err
	}

	return handleExecutionResult(result, "unbootstrap", logger)
}

func runDaemonLoop(ctx context.Context, cfg *config.Config, logger *slog.Logger, machineName string) error {
	statusFilePath := spec.StatusFilePath

	if _, err := os.Stat(statusFilePath); err == nil {
		logger.Info("Removing stale status file from previous daemon session...")
		status.RemoveStatusFileBestEffortAtPath(logger, statusFilePath)
	}

	removed, err := spec.RemoveManagedClusterSpecSnapshot()
	if err != nil {
		logger.Warn("Failed to remove stale managed cluster spec snapshot", "error", err)
	} else if removed {
		logger.Info("Removed stale managed cluster spec snapshot successfully")
	}

	logger.Info("Starting periodic status collection daemon", "statusInterval", "1m", "bootstrapCheckInterval", "2m", "specCollectionInterval", "10m")

	var cfgMu sync.RWMutex
	var bootstrapInProgress int32

	if err := collectAndWriteStatus(ctx, cfg, logger, statusFilePath, machineName); err != nil {
		logger.Error("Failed to collect initial status", "error", err)
	}

	driftEnabled := cfg != nil && cfg.IsDriftDetectionAndRemediationEnabled()
	if !driftEnabled {
		logger.Info("Drift detection and remediation is disabled by config")
	}

	var detectors []drift.Detector
	if driftEnabled {
		detectors = drift.DefaultDetectors()
		if err := collectAndWriteManagedClusterSpec(ctx, cfg, logger); err != nil {
			logger.Warn("Failed to collect initial managed cluster spec", "error", err)
		} else {
			cfgSnap := snapshotConfig(cfg, &cfgMu)
			if err := drift.DetectAndRemediateFromFiles(ctx, cfgSnap, logger, &bootstrapInProgress, detectors, machineName); err != nil {
				logger.Warn("Initial drift detection after spec collection failed", "error", err)
			} else {
				logger.Info("Initial drift detection after spec collection completed successfully")
			}
		}
	}

	var wg sync.WaitGroup
	startDaemonLoops(ctx, cfg, statusFilePath, logger, &cfgMu, &bootstrapInProgress, detectors, driftEnabled, &wg, machineName)

	<-ctx.Done()
	logger.Info("Daemon shutting down due to context cancellation")
	wg.Wait()
	return ctx.Err()
}

func startDaemonLoops(
	ctx context.Context,
	cfg *config.Config,
	statusFilePath string,
	logger *slog.Logger,
	cfgMu *sync.RWMutex,
	bootstrapInProgress *int32,
	detectors []drift.Detector,
	driftEnabled bool,
	wg *sync.WaitGroup,
	machineName string,
) {
	if wg == nil {
		return
	}
	if driftEnabled {
		wg.Add(3)
	} else {
		wg.Add(2)
	}
	startStatusCollectionLoop(ctx, cfg, statusFilePath, logger, cfgMu, wg, machineName)
	startBootstrapHealthCheckLoop(ctx, cfg, logger, cfgMu, bootstrapInProgress, wg, machineName)
	if driftEnabled {
		startNodeDriftDetectionAndRemediationLoop(ctx, cfg, logger, cfgMu, bootstrapInProgress, detectors, wg, machineName)
	}
}

func snapshotConfig(cfg *config.Config, cfgMu *sync.RWMutex) *config.Config {
	if cfg == nil {
		return nil
	}
	if cfgMu != nil {
		cfgMu.RLock()
		defer cfgMu.RUnlock()
	}
	return cfg.DeepCopy()
}

func startStatusCollectionLoop(
	ctx context.Context,
	cfg *config.Config,
	statusFilePath string,
	logger *slog.Logger,
	cfgMu *sync.RWMutex,
	wg *sync.WaitGroup,
	machineName string,
) {
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				logger.Info("Starting periodic status collection")
				cfgSnap := snapshotConfig(cfg, cfgMu)
				err := collectAndWriteStatus(ctx, cfgSnap, logger, statusFilePath, machineName)
				if err != nil {
					logger.Error("Failed to collect status", "error", err)
					continue
				}
				logger.Info("Status collection completed successfully")
			}
		}
	}()
}

func startBootstrapHealthCheckLoop(
	ctx context.Context,
	cfg *config.Config,
	logger *slog.Logger,
	cfgMu *sync.RWMutex,
	bootstrapInProgress *int32,
	wg *sync.WaitGroup,
	machineName string,
) {
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				logger.Info("Starting bootstrap health check")

				if !atomic.CompareAndSwapInt32(bootstrapInProgress, 0, 1) {
					logger.Warn("Bootstrap already in progress, skipping this interval")
					continue
				}
				func() {
					defer atomic.StoreInt32(bootstrapInProgress, 0)
					cfgSnap := snapshotConfig(cfg, cfgMu)
					err := checkAndBootstrap(ctx, cfgSnap, logger, machineName)
					if err != nil {
						logger.Error("Auto-bootstrap check failed", "error", err)
						return
					}
					logger.Info("Bootstrap health check completed")
				}()
			}
		}
	}()
}

func startNodeDriftDetectionAndRemediationLoop(
	ctx context.Context,
	cfg *config.Config,
	logger *slog.Logger,
	cfgMu *sync.RWMutex,
	bootstrapInProgress *int32,
	detectors []drift.Detector,
	wg *sync.WaitGroup,
	machineName string,
) {
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				logger.Info("Starting periodic managed cluster spec collection")
				cfgSnap := snapshotConfig(cfg, cfgMu)
				err := collectAndWriteManagedClusterSpec(ctx, cfgSnap, logger)
				if err != nil {
					logger.Warn("Failed to collect managed cluster spec", "error", err)
					continue
				}
				logger.Info("Managed cluster spec collection completed")

				if err := drift.DetectAndRemediateFromFiles(ctx, cfgSnap, logger, bootstrapInProgress, detectors, machineName); err != nil {
					logger.Warn("Drift detection after spec collection failed", "error", err)
				} else {
					logger.Info("Drift detection after spec collection completed")
				}
			}
		}
	}()
}

func collectAndWriteManagedClusterSpec(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	collector := spec.NewManagedClusterSpecCollector(cfg, logger)
	_, err := collector.Collect(ctx)
	return err
}

func checkAndBootstrap(ctx context.Context, cfg *config.Config, logger *slog.Logger, machineName string) error {
	collector := status.NewCollector(cfg, logger, Version, machineName)

	needsBootstrap := collector.NeedsBootstrap(ctx)
	if !needsBootstrap {
		return nil
	}

	logger.Info("Node requires re-bootstrapping, initiating auto-bootstrap...")

	if cfg != nil && cfg.IsDriftDetectionAndRemediationEnabled() {
		if err := collectAndWriteManagedClusterSpec(ctx, cfg, logger); err != nil {
			logger.Warn("Failed to refresh managed cluster spec before auto-bootstrap", "error", err)
		}

		if changed, oldV, newV, err := spec.OverrideKubernetesVersionFromManagedClusterSpec(cfg); err == nil && changed {
			logger.Info("Overriding Kubernetes version from managed cluster spec", "old", oldV, "new", newV)
		}
	}

	bootstrapExecutor := bootstrapper.New(cfg, logger, machineName)
	result, err := bootstrapExecutor.Bootstrap(ctx)
	if err != nil {
		status.RemoveStatusFileBestEffort(logger)
		return fmt.Errorf("auto-bootstrap failed: %w", err)
	}

	if err := handleExecutionResult(result, "auto-bootstrap", logger); err != nil {
		status.RemoveStatusFileBestEffort(logger)
		return fmt.Errorf("auto-bootstrap execution failed: %w", err)
	}

	logger.Info("Auto-bootstrap completed successfully")
	return nil
}

func collectAndWriteStatus(ctx context.Context, cfg *config.Config, logger *slog.Logger, statusFilePath string, machineName string) error {
	collector := status.NewCollector(cfg, logger, Version, machineName)

	nodeStatus, err := collector.CollectStatus(ctx)
	if err != nil {
		return fmt.Errorf("failed to collect node status: %w", err)
	}
	if nodeStatus != nil {
		nodeStatus.LastUpdatedBy = status.LastUpdatedByStatusCollectionLoop
		nodeStatus.LastUpdatedReason = status.LastUpdatedReasonPeriodicStatusLoop
	}

	err = status.WriteStatusToFile(statusFilePath, nodeStatus)
	if err != nil {
		return fmt.Errorf("failed to write status to file: %w", err)
	}
	logger.Debug("Status written", "path", statusFilePath)
	return nil
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
