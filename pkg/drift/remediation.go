package drift

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"

	"github.com/Azure/AKSFlexNode/pkg/bootstrapper"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/kube"
	"github.com/Azure/AKSFlexNode/pkg/spec"
	"github.com/Azure/AKSFlexNode/pkg/status"
	"github.com/Azure/AKSFlexNode/pkg/systemd"
)

const driftKubernetesUpgradeOperation = "drift-kubernetes-upgrade"

const (
	upgradeStepCordonAndDrain       = "cordon-and-drain"
	upgradeStepStopKubelet          = "stop-kubelet"
	upgradeStepDownloadKubeBinaries = "download-kube-binaries"
	upgradeStepStartKubelet         = "start-kubelet"
	upgradeStepUncordon             = "uncordon"
)

const agentServiceName = "aks-flex-node-agent.service"

// maxManagedClusterSpecAge is a safety guard to avoid acting on very stale spec snapshots.
// In normal operation we run drift immediately after a successful spec collection, so this
// should rarely block remediation.
const maxManagedClusterSpecAge = 2 * time.Hour

// DetectAndRemediateFromFiles loads spec/status snapshots from disk, runs all detectors,
// and (if needed) performs remediation.
//
// Remediation attempts are guarded by bootstrapInProgress to avoid concurrent executions.
//
// conn must be a usable components API connection; drift remediation never creates its own
// in-memory connection.
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

	return detectAndRemediate(ctx, cfg, logger, bootstrapInProgress, detectors, specSnap, nodeStatus, conn)
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
		result, upgradeErr := runKubernetesUpgradeRemediation(ctx, cfg, logger, conn)
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

	case RemediationActionReboot:
		if err := runRebootRemediation(ctx, logger); err != nil {
			return fmt.Errorf("reboot remediation failed: %w", err)
		}
		logger.Info("Reboot remediation completed without error")
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
	conn *grpc.ClientConn,
) (*bootstrapper.ExecutionResult, error) {
	// runKubernetesUpgradeRemediation performs a targeted Kubernetes upgrade with minimal disruption.
	//
	// Key design points:
	//   - Stop/start kubelet around the upgrade so we don't run kubelet against partially-updated
	//     binaries or config (avoids flapping, crash loops, and nondeterministic behavior).
	//   - Do not stop/restart containerd to keep disruption lower and avoid impacting running pods
	//     more than necessary.
	if conn == nil {
		return nil, errors.New("components API connection is required")
	}

	// For kubelet upgrades we cordon+drain the node first to minimize disruption.
	// We only uncordon if we cordoned the node in this remediation run.
	nodeOps := newKubeNodeMaintenance(cfg, logger)
	cordonState := &cordonDrainState{}

	steps := []bootstrapper.Executor{
		newCordonAndDrainExecutor(upgradeStepCordonAndDrain, logger, nodeOps, cordonState),
		// Stop/disable kubelet around the upgrade so we don't run kubelet against partially-updated bits.
		bootstrapper.StopKubeletExecutor(upgradeStepStopKubelet, conn, cfg),
		// Install the desired kube binaries version.
		bootstrapper.DownloadKubeBinariesExecutor(upgradeStepDownloadKubeBinaries, conn, cfg),
		// Reconfigure + start kubelet to match the upgraded bits.
		bootstrapper.StartKubeletExecutor(upgradeStepStartKubelet, conn, cfg),
		newUncordonExecutor(upgradeStepUncordon, logger, nodeOps, cordonState),
	}

	be := bootstrapper.NewBaseExecutor(cfg, logger)
	result, err := be.ExecuteSteps(ctx, steps, driftKubernetesUpgradeOperation)
	if err != nil && logger != nil {
		// Special-case: if the only thing that failed was uncordon, best-effort retry so the
		// node doesn't remain stuck unschedulable after a successful upgrade.
		if failedStepName(result) == upgradeStepUncordon {
			nodeName, hnErr := os.Hostname()
			if hnErr == nil && cordonState.shouldUncordon(nodeName) {
				logger.WithError(err).Warnf("Upgrade remediation failed at uncordon; retrying uncordon best-effort for node %s", nodeName)
				_ = nodeOps.Uncordon(ctx, nodeName)
			}
		}
	}
	return result, err
}

// rebootCommandRunner is a function that executes the reboot command and returns combined output.
// It is a field type to allow injection in tests.
type rebootCommandRunner func(ctx context.Context) ([]byte, error)

func defaultRebootRunner(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "systemctl", "reboot") // #nosec G204 -- fixed command, no user input
	return cmd.CombinedOutput()
}

func runRebootRemediation(
	ctx context.Context,
	logger *logrus.Logger,
) error {
	return runRebootRemediationWithDeps(ctx, logger, systemd.New(), defaultRebootRunner)
}

func runRebootRemediationWithDeps(
	ctx context.Context,
	logger *logrus.Logger,
	mgr systemd.Manager,
	runner rebootCommandRunner,
) error {
	// Key design points:
	//   - Only reboot if aks-flex-node-agent is running as a systemd service
	//   - If not running under systemd, skip reboot (agent may be running in development/test mode)
	//   - Use systemctl reboot for a clean shutdown
	if logger == nil {
		logger = logrus.New()
	}

	// Check if aks-flex-node-agent is managed by systemd.
	// We use GetUnitStatus to check if the service is active and running under systemd.
	unitStatus, err := mgr.GetUnitStatus(ctx, agentServiceName)
	if err != nil {
		if errors.Is(err, systemd.ErrUnitNotFound) {
			logger.Warn("aks-flex-node-agent is not running as a systemd service; skipping reboot")
			// Not running under systemd is an expected scenario (e.g., dev/test); treat as a no-op, not an error.
			return nil
		}
		logger.WithError(err).Warn("Failed to check systemd service status; aborting reboot remediation")
		return fmt.Errorf("failed to check systemd service status: %w", err)
	}

	// Only reboot if the service is active
	if unitStatus.ActiveState != systemd.UnitActiveStateActive {
		logger.Warnf("aks-flex-node-agent service is not active (state: %s); skipping reboot", unitStatus.ActiveState)
		return nil
	}

	logger.Info("Initiating system reboot via systemctl")

	// Use systemctl reboot for a clean shutdown.
	// This will gracefully stop services and sync filesystems before rebooting.
	// To avoid silently ignoring immediate failures (e.g., DBus unavailable), run the
	// command and check its exit status, using a short timeout if no deadline is set.
	rebootCtx := ctx
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		rebootCtx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}

	output, err := runner(rebootCtx)
	if err != nil {
		// If the context was canceled or timed out, surface that information explicitly.
		if errors.Is(rebootCtx.Err(), context.DeadlineExceeded) {
			logger.WithError(err).WithField("output", string(output)).
				Error("systemctl reboot timed out")
			return fmt.Errorf("systemctl reboot timed out: %w", err)
		}
		logger.WithError(err).WithField("output", string(output)).
			Error("systemctl reboot failed")
		return fmt.Errorf("systemctl reboot failed: %w", err)
	}
	return nil
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
	// Cordon/drain failures are generally control-plane/RBAC/timeouts, and uncordon failures do not
	// imply kubelet is unhealthy.
	switch failedStepName(result) {
	case upgradeStepCordonAndDrain, upgradeStepUncordon:
		return false
	case upgradeStepStopKubelet, upgradeStepDownloadKubeBinaries, upgradeStepStartKubelet:
		return true
	default:
		// Unknown step; avoid unnecessary auto-bootstrap unless we can positively identify a kubelet/binary issue.
		return false
	}
}
