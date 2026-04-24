package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"

	_ "github.com/Azure/AKSFlexNode/components"
	"github.com/Azure/AKSFlexNode/components/services/inmem"
	"github.com/Azure/AKSFlexNode/pkg/bootstrapper"
	"github.com/Azure/AKSFlexNode/pkg/config"
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
	log := logger.GetLoggerFromContext(ctx)

	if err := spec.EnsureRuntimeDir(); err != nil {
		return err
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("failed to load config from %s: %w", configPath, err)
	}

	conn, err := inmem.NewConnection()
	if err != nil {
		return fmt.Errorf("failed to create in-memory components API connection: %w", err)
	}
	defer conn.Close() //nolint:errcheck // stops in memory connection only

	slogger := slog.Default()
	machineName := goalstates.NSpawnMachineKube1

	bootstrapExecutor := bootstrapper.New(cfg, slogger, machineName)
	result, err := bootstrapExecutor.Bootstrap(ctx)
	if err != nil {
		return err
	}

	// Handle and log the bootstrap result
	if err := handleExecutionResult(result, "bootstrap", log); err != nil {
		return err
	}

	// After successful bootstrap, transition to daemon mode
	log.Info("Bootstrap completed successfully, transitioning to daemon mode...")
	return runDaemonLoop(ctx, cfg, conn, machineName)
}

// runUnbootstrap executes the unbootstrap process
func runUnbootstrap(ctx context.Context) error {
	log := logger.GetLoggerFromContext(ctx)

	if err := spec.EnsureRuntimeDir(); err != nil {
		return err
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("failed to load config from %s: %w", configPath, err)
	}

	conn, err := inmem.NewConnection()
	if err != nil {
		return fmt.Errorf("failed to create in-memory components API connection: %w", err)
	}
	defer conn.Close() //nolint:errcheck // stops in memory connection only

	slogger := slog.Default()
	machineName := goalstates.NSpawnMachineKube1

	bootstrapExecutor := bootstrapper.New(cfg, slogger, machineName)
	result, err := bootstrapExecutor.Unbootstrap(ctx)
	if err != nil {
		return err
	}

	// Handle and log the result (unbootstrap is more lenient with failures)
	return handleExecutionResult(result, "unbootstrap", log)
}

// runVersion displays version information
func runVersion() {
	fmt.Printf("AKS Flex Node Agent\n")
	fmt.Printf("Version: %s\n", Version)
	fmt.Printf("Git Commit: %s\n", GitCommit)
	fmt.Printf("Build Time: %s\n", BuildTime)
}

// runDaemonLoop runs the periodic status collection and bootstrap monitoring daemon
func runDaemonLoop(ctx context.Context, cfg *config.Config, conn *grpc.ClientConn, machineName string) error {
	logger := logger.GetLoggerFromContext(ctx)
	// Runtime directory already ensured by runAgent; get status file path.
	statusFilePath := spec.StatusFilePath

	// Clean up any stale status file on daemon startup
	if _, err := os.Stat(statusFilePath); err == nil {
		logger.Info("Removing stale status file from previous daemon session...")
		status.RemoveStatusFileBestEffortAtPath(logger, statusFilePath)
	}

	// Always remove managed cluster spec snapshot on daemon startup.
	// We'll re-collect it shortly after startup and on a schedule.
	removed, err := spec.RemoveManagedClusterSpecSnapshot()
	if err != nil {
		logger.Warnf("Failed to remove stale managed cluster spec snapshot: %v", err)
	} else if removed {
		logger.Info("Removed stale managed cluster spec snapshot successfully")
	}

	logger.Info("Starting periodic status collection daemon (status: 1 minutes, bootstrap check: 2 minute， spec collection: 10 minutes)...")

	// Protect cfg reads/writes across concurrent loops. This avoids data races when we
	// temporarily update cfg.Kubernetes.Version to trigger drift remediation bootstrap.
	var cfgMu sync.RWMutex

	// Guard to prevent overlapping bootstrap runs across loops.
	var bootstrapInProgress int32

	// Collect status immediately on start
	if err := collectAndWriteStatus(ctx, cfg, statusFilePath, machineName); err != nil {
		logger.Errorf("Failed to collect initial status: %v", err)
	}

	driftEnabled := cfg != nil && cfg.IsDriftDetectionAndRemediationEnabled()
	if !driftEnabled {
		logger.Info("Drift detection and remediation is disabled by config")
	}

	var detectors []drift.Detector
	if driftEnabled {
		// Initialize drift detectors and collect initial managed cluster spec before starting loops to ensure drift loop has what it needs to run on schedule without waiting for the first spec collection interval.
		detectors = drift.DefaultDetectors()
		// Collect managed cluster spec once on daemon startup.
		if err := collectAndWriteManagedClusterSpec(ctx, cfg); err != nil {
			logger.Warnf("Failed to collect initial managed cluster spec: %v", err)
		} else {
			cfgSnap := snapshotConfig(cfg, &cfgMu)
			if err := drift.DetectAndRemediateFromFiles(ctx, cfgSnap, logger, &bootstrapInProgress, detectors, conn, machineName); err != nil {
				logger.Warnf("Initial drift detection after spec collection failed: %v", err)
			} else {
				logger.Info("Initial drift detection after spec collection completed successfully")
			}
		}
	}

	var wg sync.WaitGroup
	startDaemonLoops(ctx, cfg, conn, statusFilePath, logger, &cfgMu, &bootstrapInProgress, detectors, driftEnabled, &wg, machineName)

	<-ctx.Done()
	logger.Info("Daemon shutting down due to context cancellation")
	wg.Wait()
	return ctx.Err()
}

func startDaemonLoops(
	ctx context.Context,
	cfg *config.Config,
	conn *grpc.ClientConn,
	statusFilePath string,
	logger *logrus.Logger,
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
	startBootstrapHealthCheckLoop(ctx, cfg, conn, logger, cfgMu, bootstrapInProgress, wg, machineName)
	if driftEnabled {
		startNodeDriftDetectionAndRemediationLoop(ctx, cfg, conn, logger, cfgMu, bootstrapInProgress, detectors, wg, machineName)
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
	logger *logrus.Logger,
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
				now := time.Now()
				logger.Infof("Starting periodic status collection at %s...", now.Format("2006-01-02 15:04:05"))
				cfgSnap := snapshotConfig(cfg, cfgMu)
				err := collectAndWriteStatus(ctx, cfgSnap, statusFilePath, machineName)
				if err != nil {
					logger.Errorf("Failed to collect status at %s: %v", now.Format("2006-01-02 15:04:05"), err)
					continue
				}
				logger.Infof("Status collection completed successfully at %s", time.Now().Format("2006-01-02 15:04:05"))
			}
		}
	}()
}

func startBootstrapHealthCheckLoop(
	ctx context.Context,
	cfg *config.Config,
	conn *grpc.ClientConn,
	logger *logrus.Logger,
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
				now := time.Now()
				logger.Infof("Starting bootstrap health check at %s...", now.Format("2006-01-02 15:04:05"))

				if !atomic.CompareAndSwapInt32(bootstrapInProgress, 0, 1) {
					logger.Warn("Bootstrap already in progress, skipping this interval")
					continue
				}
				func() {
					defer atomic.StoreInt32(bootstrapInProgress, 0)
					cfgSnap := snapshotConfig(cfg, cfgMu)
					err := checkAndBootstrap(ctx, cfgSnap, machineName)
					if err != nil {
						logger.Errorf("Auto-bootstrap check failed at %s: %v", now.Format("2006-01-02 15:04:05"), err)
						return
					}
					logger.Infof("Bootstrap health check completed at %s", time.Now().Format("2006-01-02 15:04:05"))
				}()
			}
		}
	}()
}

func startNodeDriftDetectionAndRemediationLoop(
	ctx context.Context,
	cfg *config.Config,
	conn *grpc.ClientConn,
	logger *logrus.Logger,
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
				now := time.Now()
				logger.Infof("Starting periodic managed cluster spec collection at %s...", now.Format("2006-01-02 15:04:05"))
				cfgSnap := snapshotConfig(cfg, cfgMu)
				err := collectAndWriteManagedClusterSpec(ctx, cfgSnap)
				if err != nil {
					logger.Warnf("Failed to collect managed cluster spec at %s: %v", now.Format("2006-01-02 15:04:05"), err)
					continue
				}
				logger.Infof("Managed cluster spec collection completed at %s", time.Now().Format("2006-01-02 15:04:05"))

				// Run drift detection immediately after spec is updated so we don't wait.
				if err := drift.DetectAndRemediateFromFiles(ctx, cfgSnap, logger, bootstrapInProgress, detectors, conn, machineName); err != nil {
					logger.Warnf("Drift detection after spec collection failed at %s: %v", time.Now().Format("2006-01-02 15:04:05"), err)
				} else {
					logger.Infof("Drift detection after spec collection completed at %s", time.Now().Format("2006-01-02 15:04:05"))
				}
			}
		}
	}()
}

func collectAndWriteManagedClusterSpec(ctx context.Context, cfg *config.Config) error {
	logger := logger.GetLoggerFromContext(ctx)
	collector := spec.NewManagedClusterSpecCollector(cfg, logger)
	_, err := collector.Collect(ctx)
	return err
}

// checkAndBootstrap checks if the node needs re-bootstrapping and performs it if necessary
func checkAndBootstrap(ctx context.Context, cfg *config.Config, machineName string) error {
	logger := logger.GetLoggerFromContext(ctx)
	// Create status collector to check bootstrap requirements
	collector := status.NewCollector(cfg, logger, Version, machineName)

	// Check if bootstrap is needed
	needsBootstrap := collector.NeedsBootstrap(ctx)
	if !needsBootstrap {
		return nil // All good, no action needed
	}

	logger.Info("Node requires re-bootstrapping, initiating auto-bootstrap...")

	if cfg != nil && cfg.IsDriftDetectionAndRemediationEnabled() {
		// Best-effort: refresh the managed cluster spec snapshot before attempting to
		// override Kubernetes version. This avoids falling back to an old static version
		// right after reboot (we delete the snapshot at daemon startup).
		if err := collectAndWriteManagedClusterSpec(ctx, cfg); err != nil {
			logger.Warnf("Failed to refresh managed cluster spec before auto-bootstrap: %v", err)
		}

		// Best-effort: prefer Kubernetes version from the persisted managed cluster spec snapshot.
		// This keeps auto-bootstrap aligned with the cluster desired version even if the static
		// config has an older value.
		if changed, oldV, newV, err := spec.OverrideKubernetesVersionFromManagedClusterSpec(cfg); err == nil && changed {
			logger.Infof("Overriding Kubernetes version from managed cluster spec: %q -> %q", oldV, newV)
		}
	}

	// Perform bootstrap
	slogger := slog.Default()
	bootstrapExecutor := bootstrapper.New(cfg, slogger, machineName)
	result, err := bootstrapExecutor.Bootstrap(ctx)
	if err != nil {
		// Bootstrap failed - remove status file so next check will detect the problem
		status.RemoveStatusFileBestEffort(logger)
		return fmt.Errorf("auto-bootstrap failed: %s", err)
	}

	// Handle and log the bootstrap result
	if err := handleExecutionResult(result, "auto-bootstrap", logger); err != nil {
		// Bootstrap execution failed - remove status file so next check will detect the problem
		status.RemoveStatusFileBestEffort(logger)
		return fmt.Errorf("auto-bootstrap execution failed: %s", err)
	}

	logger.Info("Auto-bootstrap completed successfully")
	return nil
}

// collectAndWriteStatus collects current node status and writes it to the status file
func collectAndWriteStatus(ctx context.Context, cfg *config.Config, statusFilePath string, machineName string) error {
	logger := logger.GetLoggerFromContext(ctx)

	// Create status collector
	collector := status.NewCollector(cfg, logger, Version, machineName)

	// Collect comprehensive status
	nodeStatus, err := collector.CollectStatus(ctx)
	if err != nil {
		return fmt.Errorf("failed to collect node status: %w", err)
	}
	if nodeStatus != nil {
		nodeStatus.LastUpdatedBy = status.LastUpdatedByStatusCollectionLoop
		nodeStatus.LastUpdatedReason = status.LastUpdatedReasonPeriodicStatusLoop
	}

	// Write status to JSON file
	err = status.WriteStatusToFile(statusFilePath, nodeStatus)
	if err != nil {
		return fmt.Errorf("failed to write status to file: %w", err)
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
