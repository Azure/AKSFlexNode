package status

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

func TestMarkKubeletUnhealthyBestEffortAtPath_CreatesOrUpdatesSnapshot(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "status.json")
	logger := logrus.New()

	now := time.Date(2026, 2, 13, 12, 0, 0, 0, time.UTC)
	MarkKubeletUnhealthyBestEffortAtPath(logger, path, now)

	snap, err := LoadStatusFromFile(path)
	if err != nil {
		t.Fatalf("LoadStatusFromFile() err=%v", err)
	}
	if snap.KubeletRunning != false {
		t.Fatalf("KubeletRunning=%v, want false", snap.KubeletRunning)
	}
	if snap.KubeletReady != "Unknown" {
		t.Fatalf("KubeletReady=%q, want %q", snap.KubeletReady, "Unknown")
	}
	if snap.KubeletVersion != "unknown" {
		t.Fatalf("KubeletVersion=%q, want %q", snap.KubeletVersion, "unknown")
	}
	if snap.LastUpdatedBy != LastUpdatedByDriftDetectionAndRemediation {
		t.Fatalf("LastUpdatedBy=%q, want %q", snap.LastUpdatedBy, LastUpdatedByDriftDetectionAndRemediation)
	}
	if snap.LastUpdatedReason != LastUpdatedReasonKubernetesVersionDrift {
		t.Fatalf("LastUpdatedReason=%q, want %q", snap.LastUpdatedReason, LastUpdatedReasonKubernetesVersionDrift)
	}
	if !snap.LastUpdated.Equal(now) {
		t.Fatalf("LastUpdated=%s, want %s", snap.LastUpdated.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	}
}
