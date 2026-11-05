package config

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
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

	// os.RemoveAll on a mount point removes everything in it, then fails to remove the
	// directory itself with EBUSY. removeAllMountPoint does the same without a mount.
	removeAllMountPoint := func(path string) error {
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := os.RemoveAll(filepath.Join(path, entry.Name())); err != nil {
				return err
			}
		}
		return &os.PathError{Op: "unlinkat", Path: path, Err: syscall.EBUSY}
	}

	t.Run("mount points are emptied", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		paths := []string{filepath.Join(root, "config"), filepath.Join(root, "logs")}
		for _, path := range paths {
			if err := os.MkdirAll(filepath.Join(path, "nested"), 0o700); err != nil {
				t.Fatalf("create test directory %s: %v", path, err)
			}
			if err := os.WriteFile(filepath.Join(path, "file"), []byte("x"), 0o600); err != nil {
				t.Fatalf("create test file in %s: %v", path, err)
			}
		}

		task := &removeRuntimeDirsTask{logger: newLogger(), paths: paths, removeAll: removeAllMountPoint}
		for run := 1; run <= 2; run++ {
			if err := task.Do(context.Background()); err != nil {
				t.Fatalf("Do() run %d error = %v", run, err)
			}
		}
		for _, path := range paths {
			entries, err := os.ReadDir(path)
			if err != nil || len(entries) != 0 {
				t.Fatalf("mount point %s: entries %v, error %v; want it kept and empty", path, entries, err)
			}
		}
	})

	t.Run("a busy directory that isn't emptied fails", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		path := filepath.Join(root, "config")
		if err := os.MkdirAll(filepath.Join(path, "nested"), 0o700); err != nil {
			t.Fatalf("create test directory: %v", err)
		}
		// As when something mounted inside the directory keeps it from being emptied.
		busy := func(path string) error {
			return &os.PathError{Op: "unlinkat", Path: filepath.Join(path, "nested"), Err: syscall.EBUSY}
		}

		task := &removeRuntimeDirsTask{logger: newLogger(), paths: []string{path}, removeAll: busy}
		if err := task.Do(context.Background()); !errors.Is(err, syscall.EBUSY) {
			t.Fatalf("Do() error = %v, want EBUSY", err)
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
