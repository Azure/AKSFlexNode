package status

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveStatusFileBestEffortAtPath_RemovesFile(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	p := filepath.Join(tmpDir, "status.json")
	if err := os.WriteFile(p, []byte(`{"foo":"bar"}`), 0o600); err != nil {
		t.Fatalf("write temp status: %v", err)
	}

	logger := slog.Default()
	RemoveStatusFileBestEffortAtPath(logger, p)

	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("expected file removed, stat err=%v", err)
	}
}

func TestRemoveStatusFileBestEffortAtPath_MissingFileNoError(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	p := filepath.Join(tmpDir, "status.json")

	logger := slog.Default()
	RemoveStatusFileBestEffortAtPath(logger, p)

	// Should remain missing.
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("expected file still missing, stat err=%v", err)
	}
}
