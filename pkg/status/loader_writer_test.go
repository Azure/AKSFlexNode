package status

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteStatusToFileAndLoadStatusFromFile_RoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "status.json")

	in := &NodeStatus{
		KubeletVersion:    "1.30.7",
		RuncVersion:       "1.1.12",
		ContainerdVersion: "1.7.20",
		KubeletRunning:    true,
		KubeletReady:      "Ready",
		ContainerdRunning: true,
		LastUpdated:       time.Now().UTC().Truncate(time.Second),
		LastUpdatedBy:     LastUpdatedByStatusCollectionLoop,
		LastUpdatedReason: LastUpdatedReasonPeriodicStatusLoop,
		AgentVersion:      "dev",
		ArcStatus:         ArcStatus{Connected: true, Registered: true, MachineName: "m"},
	}

	if err := WriteStatusToFile(path, in); err != nil {
		t.Fatalf("WriteStatusToFile() err=%v", err)
	}

	// Ensure we didn't leave a temp file around.
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Fatalf("temp file still exists")
	}

	out, err := LoadStatusFromFile(path)
	if err != nil {
		t.Fatalf("LoadStatusFromFile() err=%v", err)
	}
	if out == nil {
		t.Fatalf("LoadStatusFromFile() out=nil")
	}
	if out.KubeletVersion != in.KubeletVersion || out.RuncVersion != in.RuncVersion {
		t.Fatalf("roundtrip mismatch: got kubelet=%q runc=%q", out.KubeletVersion, out.RuncVersion)
	}
	if out.LastUpdatedBy != in.LastUpdatedBy || out.LastUpdatedReason != in.LastUpdatedReason {
		t.Fatalf("metadata mismatch: got by=%q reason=%q", out.LastUpdatedBy, out.LastUpdatedReason)
	}
}

func TestWriteStatusToFile_ValidationErrors(t *testing.T) {
	t.Parallel()

	if err := WriteStatusToFile("", &NodeStatus{}); err == nil {
		t.Fatalf("expected error for empty path")
	}
	if err := WriteStatusToFile("/tmp/does-not-matter.json", nil); err == nil {
		t.Fatalf("expected error for nil status")
	}
}

func TestLoadStatusFromFile_Errors(t *testing.T) {
	t.Parallel()

	if _, err := LoadStatusFromFile(""); err == nil {
		t.Fatalf("expected error for empty path")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "status.json")
	if err := os.WriteFile(path, []byte("not-json"), 0o600); err != nil {
		t.Fatalf("WriteFile() err=%v", err)
	}
	if _, err := LoadStatusFromFile(path); err == nil {
		t.Fatalf("expected unmarshal error")
	}
}
