package npd

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

func TestRemoveHostWithoutUnit(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	worker := none.Root{HostRoot: root}
	paths := []string{worker.Path(npdBinaryPath), worker.Path(npdConfigPath)}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("owned"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	task := RemoveHost(slog.New(slog.DiscardHandler), worker)
	for range 2 {
		if err := task.Do(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range paths {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("NPD artifact remains: %v", err)
		}
	}
}

func TestRemoveHostDisablesBeforeRemovingArtifacts(t *testing.T) {
	t.Parallel()

	root := none.Root{HostRoot: t.TempDir()}
	unit := root.Path(filepath.Join("/etc/systemd/system", systemdUnitNPD))
	if err := os.MkdirAll(filepath.Dir(unit), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unit, []byte("unit"), 0o600); err != nil {
		t.Fatal(err)
	}

	calls := 0
	task := &removeHostTask{
		log:  slog.New(slog.DiscardHandler),
		root: root,
		run: func(_ context.Context, _ *slog.Logger, command string, args ...string) (string, error) {
			calls++
			if command != "systemctl" || len(args) != 3 || args[0] != "disable" || args[1] != "--now" || args[2] != systemdUnitNPD {
				t.Errorf("unexpected command: %s %v", command, args)
			}
			if _, err := os.Stat(unit); err != nil {
				t.Errorf("unit removed before disable: %v", err)
			}
			return "", nil
		},
	}
	if err := task.Do(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("disable calls = %d, want 1", calls)
	}
	if _, err := os.Stat(unit); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unit remains after disable: %v", err)
	}
}

func TestRemoveHostPreservesArtifactsWhenDisableFails(t *testing.T) {
	t.Parallel()

	root := none.Root{HostRoot: t.TempDir()}
	unit := root.Path(filepath.Join("/etc/systemd/system", systemdUnitNPD))
	if err := os.MkdirAll(filepath.Dir(unit), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unit, []byte("unit"), 0o600); err != nil {
		t.Fatal(err)
	}

	task := &removeHostTask{
		log:  slog.New(slog.DiscardHandler),
		root: root,
		run: func(_ context.Context, _ *slog.Logger, command string, args ...string) (string, error) {
			if command != "systemctl" || len(args) != 3 || args[0] != "disable" || args[1] != "--now" || args[2] != systemdUnitNPD {
				t.Errorf("unexpected command: %s %v", command, args)
			}
			return "", errors.New("disable failed")
		},
	}
	if err := task.Do(t.Context()); err == nil {
		t.Fatal("cleanup succeeded despite failed disable")
	}
	if _, err := os.Stat(unit); err != nil {
		t.Fatalf("unit removed after failed disable: %v", err)
	}
}
