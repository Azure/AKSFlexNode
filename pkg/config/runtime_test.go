package config

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveRuntimeDirsTask(t *testing.T) {
	t.Parallel()

	newLogger := func() *slog.Logger {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	t.Run("removes existing directories", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		paths := []string{
			filepath.Join(root, "run"),
			filepath.Join(root, "config"),
			filepath.Join(root, "logs"),
		}
		for _, path := range paths {
			if err := os.MkdirAll(filepath.Join(path, "nested"), 0o700); err != nil {
				t.Fatalf("create test directory %s: %v", path, err)
			}
		}

		task := &removeRuntimeDirsTask{logger: newLogger(), paths: paths}
		if err := task.Do(context.Background()); err != nil {
			t.Fatalf("Do() error = %v", err)
		}

		for _, path := range paths {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("path %s still exists or stat failed with unexpected error: %v", path, err)
			}
		}
	})

	t.Run("missing directories are ignored", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		task := &removeRuntimeDirsTask{
			logger: newLogger(),
			paths: []string{
				filepath.Join(root, "missing-run"),
				filepath.Join(root, "missing-config"),
				filepath.Join(root, "missing-logs"),
			},
		}

		if err := task.Do(context.Background()); err != nil {
			t.Fatalf("Do() error = %v", err)
		}
	})
}

func TestRemoveRuntimeDirsTaskName(t *testing.T) {
	t.Parallel()

	task := RemoveRuntimeDirs(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if got := task.Name(); got != "remove-runtime-dirs" {
		t.Fatalf("Name() = %q, want remove-runtime-dirs", got)
	}
}
