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

func TestMarkKubeletHealthyAfterUpgradeBestEffortAtPath_UpdatesKubeletOnly(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "status.json")
	logger := logrus.New()

	// Seed a status snapshot similar to what the periodic collector would write.
	seed := &NodeStatus{
		KubeletVersion:    "1.30.0",
		RuncVersion:       "1.2.3",
		ContainerdVersion: "2.0.0",
		KubeletRunning:    false,
		KubeletReady:      "NotReady",
		ContainerdRunning: true,
		LastUpdated:       time.Date(2026, 2, 13, 11, 0, 0, 0, time.UTC),
		LastUpdatedBy:     LastUpdatedByStatusCollectionLoop,
		LastUpdatedReason: LastUpdatedReasonPeriodicStatusLoop,
		AgentVersion:      "v0",
		ArcStatus:         ArcStatus{Connected: true, MachineName: "m"},
	}
	if err := WriteStatusToFile(path, seed); err != nil {
		t.Fatalf("WriteStatusToFile() err=%v", err)
	}

	now := time.Date(2026, 2, 13, 12, 0, 0, 0, time.UTC)
	MarkKubeletHealthyAfterUpgradeBestEffortAtPath(logger, path, "1.31.0", now)

	snap, err := LoadStatusFromFile(path)
	if err != nil {
		t.Fatalf("LoadStatusFromFile() err=%v", err)
	}

	if snap.KubeletRunning != true {
		t.Fatalf("KubeletRunning=%v, want true", snap.KubeletRunning)
	}
	if snap.KubeletVersion != "1.31.0" {
		t.Fatalf("KubeletVersion=%q, want %q", snap.KubeletVersion, "1.31.0")
	}
	// Preserve KubeletReady if it was already set.
	if snap.KubeletReady != "NotReady" {
		t.Fatalf("KubeletReady=%q, want %q", snap.KubeletReady, "NotReady")
	}
	// Preserve other fields.
	if snap.RuncVersion != "1.2.3" {
		t.Fatalf("RuncVersion=%q, want %q", snap.RuncVersion, "1.2.3")
	}
	if snap.ContainerdVersion != "2.0.0" {
		t.Fatalf("ContainerdVersion=%q, want %q", snap.ContainerdVersion, "2.0.0")
	}
	if snap.ContainerdRunning != true {
		t.Fatalf("ContainerdRunning=%v, want true", snap.ContainerdRunning)
	}
	if snap.ArcStatus.MachineName != "m" || snap.ArcStatus.Connected != true {
		t.Fatalf("ArcStatus=%+v, want machineName=%q connected=true", snap.ArcStatus, "m")
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
