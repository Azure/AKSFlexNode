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

	snap, err := LoadStatusFromFile(statusFilePath)
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

	if err := WriteStatusToFile(statusFilePath, snap); err != nil {
		logger.Debugf("Failed to mark status unhealthy at %s: %v", statusFilePath, err)
	}
}
