package config

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
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
			filepath.Join(root, "sdk"),
			filepath.Join(root, "config"),
			filepath.Join(root, "logs"),
		}
		for _, path := range paths {
			if err := os.MkdirAll(filepath.Join(path, "nested"), 0o700); err != nil {
				t.Fatalf("create test directory %s: %v", path, err)
			}
			if err := os.WriteFile(filepath.Join(path, "nested", "data"), []byte("x"), 0o600); err != nil {
				t.Fatalf("create test file in %s: %v", path, err)
			}
		}

		task := &removeRuntimeDirsTask{logger: newLogger(), paths: paths}
		for run := 1; run <= 2; run++ {
			if err := task.Do(context.Background()); err != nil {
				t.Fatalf("Do() run %d error = %v", run, err)
			}
		}

		for _, path := range paths {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("path %s still exists or stat failed with unexpected error: %v", path, err)
			}
		}
	})

	t.Run("SDK cleanup failure preserves Flex state", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		sdkDir := filepath.Join(root, "sdk")
		configDir := filepath.Join(root, "config")
		logDir := filepath.Join(root, "logs")
		if err := os.MkdirAll(configDir, 0o700); err != nil {
			t.Fatal(err)
		}

		files := []string{
			filepath.Join(configDir, "config.json"),
			filepath.Join(configDir, "daemon-state.json"),
			filepath.Join(configDir, "daemon-state.json.sha256"),
		}
		for _, path := range files {
			if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
				t.Fatal(err)
			}
		}

		removeErr := errors.New("SDK directory removal failed")
		var removed []string
		task := &removeRuntimeDirsTask{
			logger: newLogger(),
			paths:  []string{sdkDir, configDir, logDir},
			removeAll: func(path string) error {
				removed = append(removed, path)
				if path == sdkDir {
					return removeErr
				}
				return os.RemoveAll(path)
			},
		}
		if err := task.Do(context.Background()); !errors.Is(err, removeErr) {
			t.Fatalf("Do() error = %v, want SDK removal failure", err)
		}
		if !slices.Equal(removed, []string{sdkDir}) {
			t.Fatalf("removal calls = %v, want only %s", removed, sdkDir)
		}
		for _, path := range files {
			if data, err := os.ReadFile(path); err != nil || string(data) != "data" {
				t.Fatalf("file %s = %q, error = %v; want retained data", path, data, err)
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

	cleanup, ok := task.(*removeRuntimeDirsTask)
	if !ok {
		t.Fatalf("task type = %T, want *removeRuntimeDirsTask", task)
	}
	want := []string{goalstates.AgentConfigDir, ConfigDir, DefaultLogDir}
	if !slices.Equal(cleanup.paths, want) {
		t.Fatalf("paths = %v, want %v", cleanup.paths, want)
	}
}
