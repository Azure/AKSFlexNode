package drift

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"

	"github.com/Azure/AKSFlexNode/pkg/bootstrapper"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/kube"
	"github.com/Azure/AKSFlexNode/pkg/spec"
	"github.com/Azure/AKSFlexNode/pkg/status"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/host"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestop"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

const driftKubernetesUpgradeOperation = "drift-kubernetes-upgrade"

const (
	upgradeStepCordonAndDrain       = "cordon-and-drain"
	upgradeStepStopKubelet          = "stop-kubelet"
	upgradeStepDownloadKubeBinaries = "download-kube-binaries"
	upgradeStepStartKubelet         = "start-kubelet"
	upgradeStepUncordon             = "uncordon"
)

// maxManagedClusterSpecAge is a safety guard to avoid acting on very stale spec snapshots.
// In normal operation we run drift immediately after a successful spec collection, so this
// should rarely block remediation.
const maxManagedClusterSpecAge = 2 * time.Hour

// DetectAndRemediateFromFiles loads spec/status snapshots from disk, runs all detectors,
// and (if needed) performs remediation.
//
// Remediation attempts are guarded by bootstrapInProgress to avoid concurrent executions.
//
// conn must be a usable components API connection; drift remediation uses it for Azure-specific
// component actions (NPD).
func DetectAndRemediateFromFiles(
	ctx context.Context,
	// cfg must be an immutable snapshot for the duration of this call.
	// DetectAndRemediateFromFiles may mutate cfg (e.g., to apply desired KubernetesVersion)
	// as part of remediation.
	cfg *config.Config,
	logger *logrus.Logger,
	bootstrapInProgress *int32,
	detectors []Detector,
	conn *grpc.ClientConn,
	machineName string,
) error {
	if logger == nil {
		logger = logrus.New()
	}

	specSnap, err := spec.LoadManagedClusterSpec()
	if err != nil {
		// Spec may not exist yet.
		return err
	}

	nodeStatus, err := status.LoadStatus()
	if err != nil {
		return err
	}

	return detectAndRemediate(ctx, cfg, logger, bootstrapInProgress, detectors, specSnap, nodeStatus, conn, machineName)
}

func detectAndRemediate(
	ctx context.Context,
	cfg *config.Config,
	logger *logrus.Logger,
	bootstrapInProgress *int32,
	detectors []Detector,
	specSnap *spec.ManagedClusterSpec,
	statusSnap *status.NodeStatus,
	conn *grpc.ClientConn,
	machineName string,
) error {
	if specSnap == nil || statusSnap == nil {
		return nil
	}
	if isManagedClusterSpecStale(specSnap, time.Now()) {
		logger.Warnf("Managed cluster spec snapshot is stale (collectedAt=%s); skipping drift remediation", specSnap.CollectedAt.Format(time.RFC3339))
		return nil
	}

	var findings []Finding
	var detectErr error
	findings, detectErr = DetectAll(ctx, detectors, cfg, specSnap, statusSnap)
	if detectErr != nil {
		// Don't immediately fail; if some detectors produced findings we can still act.
		logger.Warnf("One or more drift detectors failed: %v", detectErr)
	}
	if len(findings) == 0 {
		return detectErr
	}

	for _, f := range findings {
		logger.Warnf("Drift detected: id=%s title=%s details=%s", f.ID, f.Title, f.Details)
	}

	plan, requiresRemediation, err := resolveRemediationPlan(findings)
	if err != nil {
		return err
	}
	if !requiresRemediation {
		return detectErr
	}

	// Prevent overlapping remediation runs.
	if bootstrapInProgress != nil {
		if !atomic.CompareAndSwapInt32(bootstrapInProgress, 0, 1) {
			logger.Warn("Bootstrap already in progress, skipping drift remediation")
			return nil
		}
		defer atomic.StoreInt32(bootstrapInProgress, 0)
	}

	if plan.DesiredKubernetesVersion != "" {
		// Apply desired version to the snapshot so remediation uses the expected kube binaries.
		if cfg != nil {
			cfg.Kubernetes.Version = plan.DesiredKubernetesVersion
		}
	}

	// Run remediation.
	switch plan.Action {
	case RemediationActionKubernetesUpgrade:
		result, upgradeErr := runKubernetesUpgradeRemediation(ctx, cfg, logger, conn, machineName)
		if upgradeErr != nil {
			if shouldMarkKubeletUnhealthyAfterUpgradeFailure(result, upgradeErr) {
				status.MarkKubeletUnhealthyBestEffort(logger)
			}
			return fmt.Errorf("kubernetes upgrade remediation failed: %w", upgradeErr)
		}
		if err := handleExecutionResult(result, driftKubernetesUpgradeOperation, logger); err != nil {
			if shouldMarkKubeletUnhealthyAfterUpgradeFailure(result, err) {
				status.MarkKubeletUnhealthyBestEffort(logger)
			}
			return fmt.Errorf("kubernetes upgrade remediation execution failed: %w", err)
		}
		// Best-effort: reflect the successful upgrade immediately in the status snapshot so
		// subsequent health checks don't rely solely on the periodic status collector.
		// Also invalidate any cached kubelet clientset so readiness checks pick up rotated kubeconfig/certs.
		kube.InvalidateKubeletClientset()
		kubeletVersion := plan.DesiredKubernetesVersion
		if kubeletVersion == "" && cfg != nil {
			kubeletVersion = cfg.Kubernetes.Version
		}
		status.MarkKubeletHealthyAfterUpgradeBestEffort(logger, kubeletVersion)
		logger.Info("Kubernetes upgrade remediation completed successfully")
		return detectErr

	default:
		return fmt.Errorf("unsupported drift remediation action: %q", plan.Action)
	}
}

func isManagedClusterSpecStale(specSnap *spec.ManagedClusterSpec, now time.Time) bool {
	if specSnap == nil {
		return true
	}
	if specSnap.CollectedAt.IsZero() {
		return true
	}
	if now.IsZero() {
		now = time.Now()
	}
	return now.Sub(specSnap.CollectedAt) > maxManagedClusterSpecAge
}

type remediationPlan struct {
	Action                   RemediationAction
	DesiredKubernetesVersion string
}

// resolveRemediationPlan collapses potentially many drift findings into a single remediation plan.
func resolveRemediationPlan(findings []Finding) (remediationPlan, bool, error) {
	plan := remediationPlan{Action: RemediationActionUnspecified}
	requiresRemediation := false

	for _, f := range findings {
		action := f.Remediation.Action
		if action == RemediationActionUnspecified {
			continue
		}

		requiresRemediation = true
		if plan.Action == RemediationActionUnspecified {
			plan.Action = action
		} else if plan.Action != action {
			return remediationPlan{}, false, errors.New("conflicting drift remediation: multiple remediation actions")
		}

		version := f.Remediation.KubernetesVersion
		if version == "" {
			continue
		}
		if plan.DesiredKubernetesVersion == "" {
			plan.DesiredKubernetesVersion = version
			continue
		}
		if plan.DesiredKubernetesVersion != version {
			return remediationPlan{}, false, errors.New("conflicting drift remediation: multiple desired Kubernetes versions")
		}
	}

	return plan, requiresRemediation, nil
}

func runKubernetesUpgradeRemediation(
	ctx context.Context,
	cfg *config.Config,
	logger *logrus.Logger,
	conn *grpc.ClientConn,
	machineName string,
) (*bootstrapper.ExecutionResult, error) {
	// Kubernetes upgrade using the nspawn model:
	// 1. Cordon + drain the node
	// 2. Stop the nspawn machine (graceful kubelet+containerd stop)
	// 3. Re-provision rootfs with new binaries
	// 4. Start the nspawn machine with new binaries
	// 5. Uncordon the node
	if conn == nil {
		return nil, errors.New("components API connection is required")
	}

	log := slog.Default()

	// Convert config to agent config and resolve goal states for the new version.
	agentCfg := config.ToAgentConfig(cfg, machineName)
	gs, err := goalstates.ResolveMachine(log, agentCfg, machineName)
	if err != nil {
		return nil, fmt.Errorf("resolve goal state for upgrade: %w", err)
	}

	nodeOps := newKubeNodeMaintenance(cfg, logger)
	cordonState := &cordonDrainState{}

	start := time.Now()

	// Step 1: Cordon and drain
	hostname, _ := os.Hostname()
	if hostname != "" {
		logger.Infof("Cordoning and draining node %s", hostname)
		if err := cordonAndDrain(ctx, logger, nodeOps, cordonState); err != nil {
			return &bootstrapper.ExecutionResult{
				Success:     false,
				Duration:    time.Since(start),
				StepResults: []bootstrapper.StepResult{{StepName: "cordon-and-drain", Success: false, Error: err.Error()}},
				Error:       err.Error(),
			}, err
		}
	}

	// Step 2: Stop the nspawn machine
	if err := nodestop.StopNode(log, machineName).Do(ctx); err != nil {
		result := &bootstrapper.ExecutionResult{
			Success:     false,
			Duration:    time.Since(start),
			StepResults: []bootstrapper.StepResult{{StepName: "stop-kubelet", Success: false, Error: err.Error()}},
			Error:       err.Error(),
		}
		return result, err
	}

	// Step 3: Re-provision rootfs with new binaries
	// TODO: allow customizing containerd, runc, and CNI plugin versions.
	provisionTask := phases.Serial(log,
		host.InstallPackages(log),
		rootfs.Provision(log, gs.RootFS),
	)
	if err := provisionTask.Do(ctx); err != nil {
		result := &bootstrapper.ExecutionResult{
			Success:     false,
			Duration:    time.Since(start),
			StepResults: []bootstrapper.StepResult{{StepName: "download-kube-binaries", Success: false, Error: err.Error()}},
			Error:       err.Error(),
		}
		return result, err
	}

	// Step 4: Start the nspawn machine with new config
	if err := nodestart.StartNode(log, gs.NodeStart).Do(ctx); err != nil {
		result := &bootstrapper.ExecutionResult{
			Success:     false,
			Duration:    time.Since(start),
			StepResults: []bootstrapper.StepResult{{StepName: "start-kubelet", Success: false, Error: err.Error()}},
			Error:       err.Error(),
		}
		return result, err
	}

	// Step 5: Uncordon
	if hostname != "" && cordonState.shouldUncordon(hostname) {
		if err := nodeOps.Uncordon(ctx, hostname); err != nil {
			logger.Warnf("Failed to uncordon node %s: %v", hostname, err)
			result := &bootstrapper.ExecutionResult{
				Success:     false,
				Duration:    time.Since(start),
				StepResults: []bootstrapper.StepResult{{StepName: "uncordon", Success: false, Error: err.Error()}},
				Error:       err.Error(),
			}
			// Best-effort retry
			_ = nodeOps.Uncordon(ctx, hostname)
			return result, err
		}
	}

	return &bootstrapper.ExecutionResult{
		Success:  true,
		Duration: time.Since(start),
	}, nil
}

// cordonAndDrain performs the cordon-and-drain step using the existing executor logic.
func cordonAndDrain(ctx context.Context, logger *logrus.Logger, nodeOps *kubeNodeMaintenance, state *cordonDrainState) error {
	executor := newCordonAndDrainExecutor("cordon-and-drain", logger, nodeOps, state)
	return executor.Execute(ctx)
}

// handleExecutionResult mirrors main's handleExecutionResult but lives in drift so remediation
// can share the same logging and error semantics.
func handleExecutionResult(result *bootstrapper.ExecutionResult, operation string, logger *logrus.Logger) error {
	if result == nil {
		return fmt.Errorf("%s result is nil", operation)
	}

	if result.Success {
		logger.Infof("%s completed successfully (duration: %v, steps: %d)",
			operation, result.Duration, result.StepCount)
		return nil
	}

	return fmt.Errorf("%s failed: %s", operation, result.Error)
}

func failedStepName(result *bootstrapper.ExecutionResult) string {
	if result == nil {
		return ""
	}
	for _, sr := range result.StepResults {
		if !sr.Success {
			return sr.StepName
		}
	}
	return ""
}

func shouldMarkKubeletUnhealthyAfterUpgradeFailure(result *bootstrapper.ExecutionResult, upgradeErr error) bool {
	if upgradeErr == nil {
		return false
	}
	// Only mark kubelet unhealthy when the failure indicates kubelet/binaries are likely in a bad state.
	switch failedStepName(result) {
	case "cordon-and-drain", "uncordon":
		return false
	case "stop-kubelet", "download-kube-binaries", "start-kubelet":
		return true
	default:
		return false
	}
}
