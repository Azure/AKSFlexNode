package status

import (
	"time"

	"github.com/sirupsen/logrus"
)

// MarkKubeletUnhealthyBestEffort updates the existing status snapshot (or creates a minimal one)
// to clearly indicate the kubelet is unhealthy.
//
// This is intended to influence NeedsBootstrap() without deleting the entire status file.
func MarkKubeletUnhealthyBestEffort(logger *logrus.Logger) {
	if logger == nil {
		logger = logrus.New()
	}

	statusFilePath := GetStatusFilePath()
	MarkKubeletUnhealthyBestEffortAtPath(logger, statusFilePath, time.Time{})
}

// MarkKubeletUnhealthyBestEffortAtPath is the path-based variant used by tests and any callers
// that want to control where the status snapshot is written.
//
// If now is zero, time.Now() is used.
func MarkKubeletUnhealthyBestEffortAtPath(logger *logrus.Logger, statusFilePath string, now time.Time) {
	if logger == nil {
		logger = logrus.New()
	}
	if statusFilePath == "" {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}

	_ = withStatusFileLock(func() error {
		snap, err := loadStatusFromFileUnlocked(statusFilePath)
		if err != nil || snap == nil {
			snap = &NodeStatus{}
		}

		// Make the status clearly unhealthy so NeedsBootstrap() will trigger.
		snap.KubeletRunning = false
		snap.KubeletReady = "Unknown"
		snap.KubeletVersion = "unknown"
		snap.LastUpdatedBy = LastUpdatedByDriftDetectionAndRemediation
		snap.LastUpdatedReason = LastUpdatedReasonKubernetesVersionDrift
		snap.LastUpdated = now

		if err := writeStatusToFileUnlocked(statusFilePath, snap); err != nil {
			logger.Debugf("Failed to mark status unhealthy at %s: %v", statusFilePath, err)
		}
		return nil
	})
}

// MarkKubeletHealthyAfterUpgradeBestEffort updates the existing status snapshot to reflect
// that kubelet should now be running with the desired version after a successful upgrade.
//
// This is intended to reduce reliance on the periodic status collection loop and to avoid
// triggering unnecessary auto-bootstrap due to stale/unknown kubelet status.
//
// It preserves other status fields (e.g., runc/containerd versions, Arc status).
func MarkKubeletHealthyAfterUpgradeBestEffort(logger *logrus.Logger, kubeletVersion string) {
	if logger == nil {
		logger = logrus.New()
	}

	statusFilePath := GetStatusFilePath()
	MarkKubeletHealthyAfterUpgradeBestEffortAtPath(logger, statusFilePath, kubeletVersion, time.Time{})
}

// MarkKubeletHealthyAfterUpgradeBestEffortAtPath is the path-based variant used by tests.
//
// If now is zero, time.Now() is used.
func MarkKubeletHealthyAfterUpgradeBestEffortAtPath(logger *logrus.Logger, statusFilePath string, kubeletVersion string, now time.Time) {
	if logger == nil {
		logger = logrus.New()
	}
	if statusFilePath == "" {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}

	_ = withStatusFileLock(func() error {
		snap, err := loadStatusFromFileUnlocked(statusFilePath)
		if err != nil || snap == nil {
			// Status file should generally exist, but avoid failing hard; create a minimal snapshot.
			snap = &NodeStatus{}
		}

		snap.KubeletRunning = true
		if kubeletVersion != "" {
			snap.KubeletVersion = kubeletVersion
		}
		if snap.KubeletReady == "" {
			snap.KubeletReady = "Unknown"
		}

		snap.LastUpdatedBy = LastUpdatedByDriftDetectionAndRemediation
		snap.LastUpdatedReason = LastUpdatedReasonKubernetesVersionDrift
		snap.LastUpdated = now

		if err := writeStatusToFileUnlocked(statusFilePath, snap); err != nil {
			logger.Debugf("Failed to mark status healthy after upgrade at %s: %v", statusFilePath, err)
		}
		return nil
	})
}
