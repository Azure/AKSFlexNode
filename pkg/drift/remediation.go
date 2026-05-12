package drift

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/Azure/AKSFlexNode/pkg/bootstrapper"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/kube"
	"github.com/Azure/AKSFlexNode/pkg/spec"
	"github.com/Azure/AKSFlexNode/pkg/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const (
	driftKubernetesUpgradeOperation = "drift-kubernetes-upgrade"
	upgradeStepCordonAndDrain       = "cordon-and-drain"
	upgradeStepNodeRepave           = "node-repave"
	upgradeStepUncordon             = "uncordon"
)

// maxManagedClusterSpecAge is a safety guard to avoid acting on very stale spec snapshots.
const maxManagedClusterSpecAge = 2 * time.Hour

const (
	waitForNodeBeforeCordonTimeout  = 3 * time.Minute
	waitForNodeBeforeCordonInterval = 10 * time.Second
)

// DetectAndRemediateFromFiles loads spec/status snapshots from disk, runs all detectors,
// and (if needed) performs remediation.
func DetectAndRemediateFromFiles(
	ctx context.Context,
	cfg *config.Config,
	logger *slog.Logger,
	bootstrapInProgress *int32,
	detectors []Detector,
) error {
	specSnap, err := spec.LoadManagedClusterSpec()
	if err != nil {
		return err
	}

	nodeStatus, err := status.LoadStatus()
	if err != nil {
		return err
	}

	return detectAndRemediate(ctx, cfg, logger, bootstrapInProgress, detectors, specSnap, nodeStatus)
}

func detectAndRemediate(
	ctx context.Context,
	cfg *config.Config,
	logger *slog.Logger,
	bootstrapInProgress *int32,
	detectors []Detector,
	specSnap *spec.ManagedClusterSpec,
	statusSnap *status.NodeStatus,
) error {
	if specSnap == nil || statusSnap == nil {
		return nil
	}
	if isManagedClusterSpecStale(specSnap, time.Now()) {
		logger.Warn("Managed cluster spec snapshot is stale; skipping drift remediation", "collectedAt", specSnap.CollectedAt.Format(time.RFC3339))
		return nil
	}

	var findings []Finding
	var detectErr error
	findings, detectErr = DetectAll(ctx, detectors, cfg, specSnap, statusSnap)
	if detectErr != nil {
		logger.Warn("One or more drift detectors failed", "error", detectErr)
	}
	if len(findings) == 0 {
		return detectErr
	}

	for _, f := range findings {
		logger.Warn("Drift detected", "id", f.ID, "title", f.Title, "details", f.Details)
	}

	plan, requiresRemediation, err := resolveRemediationPlan(findings)
	if err != nil {
		return err
	}
	if !requiresRemediation {
		return detectErr
	}

	if bootstrapInProgress != nil {
		if !atomic.CompareAndSwapInt32(bootstrapInProgress, 0, 1) {
			logger.Warn("Bootstrap already in progress, skipping drift remediation")
			return nil
		}
		defer atomic.StoreInt32(bootstrapInProgress, 0)
	}

	if plan.DesiredKubernetesVersion != "" {
		if cfg != nil {
			cfg.Kubernetes.Version = plan.DesiredKubernetesVersion
		}
	}

	switch plan.Action {
	case RemediationActionKubernetesUpgrade:
		result, upgradeErr := runKubernetesUpgradeRemediation(ctx, cfg, logger)
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
	logger *slog.Logger,
) (*bootstrapper.ExecutionResult, error) {
	nodeOps := newKubeNodeMaintenance(cfg, logger)
	cordonState := &cordonDrainState{}

	start := time.Now()

	// Step 1: Cordon and drain
	hostname, _ := os.Hostname()
	if hostname != "" {
		// Workaround for a startup race where the drift loop can detect a kubelet
		// version mismatch before kubelet has registered this host as a Kubernetes
		// Node. Without this guard, cordon fails with NotFound; remediation later
		// succeeds on the periodic retry. This will be reworked in a follow-up PR.
		waitForNodeBeforeCordon(ctx, logger, nodeOps, hostname)
		logger.Info("Cordoning and draining node", "node", hostname)
		if err := cordonAndDrain(ctx, logger, nodeOps, cordonState); err != nil {
			return &bootstrapper.ExecutionResult{
				Success:     false,
				Duration:    time.Since(start),
				StepResults: []bootstrapper.StepResult{{StepName: "cordon-and-drain", Success: false, Error: err.Error()}},
				Error:       err.Error(),
			}, err
		}
	}

	if err := bootstrapper.Repave(ctx, cfg, logger); err != nil {
		result := &bootstrapper.ExecutionResult{
			Success:     false,
			Duration:    time.Since(start),
			StepResults: []bootstrapper.StepResult{{StepName: upgradeStepNodeRepave, Success: false, Error: err.Error()}},
			Error:       err.Error(),
		}
		return result, err
	}

	// Step 2: Uncordon
	if hostname != "" && cordonState.shouldUncordon(hostname) {
		if err := nodeOps.Uncordon(ctx, hostname); err != nil {
			logger.Warn("Failed to uncordon node", "node", hostname, "error", err)
			result := &bootstrapper.ExecutionResult{
				Success:     false,
				Duration:    time.Since(start),
				StepResults: []bootstrapper.StepResult{{StepName: "uncordon", Success: false, Error: err.Error()}},
				Error:       err.Error(),
			}
			_ = nodeOps.Uncordon(ctx, hostname)
			return result, err
		}
	}

	return &bootstrapper.ExecutionResult{
		Success:  true,
		Duration: time.Since(start),
	}, nil
}

func waitForNodeBeforeCordon(ctx context.Context, logger *slog.Logger, nodeOps *kubeNodeMaintenance, nodeName string) {
	deadline := time.NewTimer(waitForNodeBeforeCordonTimeout)
	defer deadline.Stop()

	ticker := time.NewTicker(waitForNodeBeforeCordonInterval)
	defer ticker.Stop()

	for {
		_, err := nodeOps.IsCordoned(ctx, nodeName)
		if err == nil {
			return
		}
		if !apierrors.IsNotFound(err) {
			logger.Debug("Node lookup before cordon failed; proceeding with cordon", "node", nodeName, "error", err)
			return
		}

		select {
		case <-ctx.Done():
			logger.Warn("Context canceled while waiting for node before cordon; proceeding with cordon", "node", nodeName, "error", ctx.Err())
			return
		case <-deadline.C:
			logger.Warn("Node did not appear before cordon timeout; proceeding with cordon", "node", nodeName, "timeout", waitForNodeBeforeCordonTimeout)
			return
		case <-ticker.C:
		}
	}
}

func cordonAndDrain(ctx context.Context, logger *slog.Logger, nodeOps *kubeNodeMaintenance, state *cordonDrainState) error {
	executor := newCordonAndDrainExecutor("cordon-and-drain", logger, nodeOps, state)
	return executor.Execute(ctx)
}

func handleExecutionResult(result *bootstrapper.ExecutionResult, operation string, logger *slog.Logger) error {
	if result == nil {
		return fmt.Errorf("%s result is nil", operation)
	}

	if result.Success {
		logger.Info("operation completed successfully", "operation", operation, "duration", result.Duration, "steps", result.StepCount)
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
	switch failedStepName(result) {
	case upgradeStepCordonAndDrain, upgradeStepUncordon:
		return false
	case upgradeStepNodeRepave:
		return true
	default:
		return false
	}
}
