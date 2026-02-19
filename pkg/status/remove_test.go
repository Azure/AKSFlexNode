package status

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sirupsen/logrus"
)

func TestRemoveStatusFileBestEffortAtPath_RemovesFile(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	p := filepath.Join(tmpDir, "status.json")
	if err := os.WriteFile(p, []byte(`{"foo":"bar"}`), 0o600); err != nil {
		t.Fatalf("write temp status: %v", err)
	}

	logger := logrus.New()
	RemoveStatusFileBestEffortAtPath(logger, p)

	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("expected file removed, stat err=%v", err)
	}
}

func TestRemoveStatusFileBestEffortAtPath_MissingFileNoError(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	p := filepath.Join(tmpDir, "status.json")

	logger := logrus.New()
	RemoveStatusFileBestEffortAtPath(logger, p)

	// Should remain missing.
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("expected file still missing, stat err=%v", err)
	}
}
