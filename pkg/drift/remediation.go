package drift

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"

	"go.goms.io/aks/AKSFlexNode/pkg/bootstrapper"
	"go.goms.io/aks/AKSFlexNode/pkg/components/kube_binaries"
	"go.goms.io/aks/AKSFlexNode/pkg/components/kubelet"
	"go.goms.io/aks/AKSFlexNode/pkg/components/services"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/spec"
	"go.goms.io/aks/AKSFlexNode/pkg/status"
)

const driftKubernetesUpgradeOperation = "drift-kubernetes-upgrade"

// maxManagedClusterSpecAge is a safety guard to avoid acting on very stale spec snapshots.
// In normal operation we run drift immediately after a successful spec collection, so this
// should rarely block remediation.
const maxManagedClusterSpecAge = 2 * time.Hour

// DetectAndRemediateFromFiles loads spec/status snapshots from disk, runs all detectors,
// and (if needed) performs remediation.
//
// Remediation attempts are guarded by bootstrapInProgress to avoid concurrent executions.
func DetectAndRemediateFromFiles(
	ctx context.Context,
	// cfg must be an immutable snapshot for the duration of this call.
	// DetectAndRemediateFromFiles may mutate cfg (e.g., to apply desired KubernetesVersion)
	// as part of remediation.
	cfg *config.Config,
	logger *logrus.Logger,
	bootstrapInProgress *int32,
	detectors []Detector,
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

	return detectAndRemediate(ctx, cfg, logger, bootstrapInProgress, detectors, specSnap, nodeStatus)
}

func detectAndRemediate(
	ctx context.Context,
	cfg *config.Config,
	logger *logrus.Logger,
	bootstrapInProgress *int32,
	detectors []Detector,
	specSnap *spec.ManagedClusterSpec,
	statusSnap *status.NodeStatus,
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
		result, upgradeErr := runKubernetesUpgradeRemediation(ctx, cfg, logger)
		if upgradeErr != nil {
			status.MarkKubeletUnhealthyBestEffort(logger)
			return fmt.Errorf("kubernetes upgrade remediation failed: %w", upgradeErr)
		}
		if err := handleExecutionResult(result, driftKubernetesUpgradeOperation, logger); err != nil {
			status.MarkKubeletUnhealthyBestEffort(logger)
			return fmt.Errorf("kubernetes upgrade remediation execution failed: %w", err)
		}
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
//
// Today the remediation runner supports executing only one remediation action per pass.
// As more detectors are added, it's possible to receive multiple findings at once. This helper
// performs two tasks:
//  1. Dedup: pick a single action and a single set of parameters (e.g., Kubernetes version).
//  2. Consistency check: if findings disagree (different actions or different desired versions),
//     fail fast rather than guessing.
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
) (*bootstrapper.ExecutionResult, error) {
	// runKubernetesUpgradeRemediation performs a targeted Kubernetes upgrade with minimal disruption.
	//
	// Key design points:
	//   - Stop/start kubelet around the upgrade so we don't run kubelet against partially-updated
	//     binaries or config (avoids flapping, crash loops, and nondeterministic behavior).
	//   - Do not stop/restart containerd to keep disruption lower and avoid impacting running pods
	//     more than necessary.
	steps := []bootstrapper.Executor{
		// Stop/disable kubelet only so it cannot restart mid-upgrade.
		services.NewKubeletOnlyUnInstaller(logger),
		// Install the desired kube binaries version.
		kube_binaries.NewInstallerWithConfig(cfg, logger),
		// Reconfigure kubelet to match the upgraded bits.
		kubelet.NewInstallerWithConfig(cfg, logger),
		// Enable/start kubelet only and wait for it to be active.
		services.NewKubeletOnlyInstaller(logger),
	}

	be := bootstrapper.NewBaseExecutor(cfg, logger)
	return be.ExecuteSteps(ctx, steps, driftKubernetesUpgradeOperation)
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
