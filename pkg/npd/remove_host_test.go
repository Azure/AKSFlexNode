package npd

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

func renderHostUnit(t *testing.T, root none.Root) string {
	t.Helper()

	start, ok := Start(slog.New(slog.DiscardHandler), &config.Config{Npd: config.NPDConfig{Enabled: true}}, root, &goalstates.NodeStart{
		NodeName: "worker-1",
		Kubelet: goalstates.Kubelet{
			APIServer: "https://example.test:443",
		},
	}, isolation.ModeNone).(*startTask)
	if !ok {
		t.Fatal("Start did not return *startTask")
	}
	if _, err := start.ensureServiceFile(); err != nil {
		t.Fatalf("render NPD unit: %v", err)
	}
	return root.Path(filepath.Join("/etc/systemd/system", systemdUnitNPD))
}

func writeNPDFile(t *testing.T, path string, contents string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readNPDFile(t *testing.T, path string) string {
	t.Helper()

	contents, err := os.ReadFile(path) //nolint:gosec // test paths are under TempDir
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func newTestRemoveHost(t *testing.T, root none.Root, run func(context.Context, *slog.Logger, string, ...string) (string, error)) *removeHostTask {
	t.Helper()

	task, ok := RemoveHost(slog.New(slog.DiscardHandler), root).(*removeHostTask)
	if !ok {
		t.Fatal("RemoveHost did not return *removeHostTask")
	}
	task.run = run
	return task
}

func checkDisableCommand(t *testing.T, command string, args []string) {
	t.Helper()

	if command != "systemctl" || !reflect.DeepEqual(args, []string{"disable", "--now", systemdUnitNPD}) {
		t.Errorf("unexpected command: %s %v", command, args)
	}
}

func assertNPDAbsent(t *testing.T, paths ...string) {
	t.Helper()

	for _, path := range paths {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("NPD artifact %s still exists or cannot be inspected: %v", path, err)
		}
	}
}

func TestRemoveHostWithoutUnit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		partial bool
	}{
		{
			name: "offline bootstrap",
		},
		{
			name:    "partial installation",
			partial: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := none.Root{HostRoot: t.TempDir()}
			unit := root.Path(filepath.Join("/etc/systemd/system", systemdUnitNPD))
			binary := root.Path(npdBinaryPath)
			config := root.Path(npdConfigPath)

			if tt.partial {
				writeNPDFile(t, binary, "binary")
				writeNPDFile(t, config, "config")
			}

			calls := 0
			task := newTestRemoveHost(t, root, func(_ context.Context, _ *slog.Logger, _ string, _ ...string) (string, error) {
				calls++
				return "", nil
			})
			for range 2 {
				if err := task.Do(t.Context()); err != nil {
					t.Fatal(err)
				}
				assertNPDAbsent(t, unit, binary, config)
			}

			if calls != 0 {
				t.Errorf("systemctl calls = %d, want 0", calls)
			}
		})
	}
}

func TestRemoveHostDisablesBeforeRemovingArtifacts(t *testing.T) {
	t.Parallel()

	root := none.Root{HostRoot: filepath.Join(t.TempDir(), "host.[worker]+")}
	unit := renderHostUnit(t, root)
	binary := root.Path(npdBinaryPath)
	config := root.Path(npdConfigPath)
	writeNPDFile(t, binary, "binary")
	writeNPDFile(t, config, "config")

	calls := 0
	task := newTestRemoveHost(t, root, func(_ context.Context, _ *slog.Logger, command string, args ...string) (string, error) {
		calls++
		checkDisableCommand(t, command, args)
		for _, path := range []string{unit, binary, config} {
			if _, err := os.Lstat(path); err != nil {
				t.Errorf("artifact %s removed before disable: %v", path, err)
			}
		}
		return "", nil
	})
	for range 2 {
		if err := task.Do(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("disable calls = %d, want 1", calls)
	}
	assertNPDAbsent(t, unit, binary, config)
}

func TestRemoveHostReturnsUnitInspectionError(t *testing.T) {
	t.Parallel()

	root := none.Root{HostRoot: t.TempDir()}
	writeNPDFile(t, root.Path("/etc/systemd/system"), "not a directory")
	config := root.Path(npdConfigPath)
	writeNPDFile(t, config, "config")

	calls := 0
	task := newTestRemoveHost(t, root, func(_ context.Context, _ *slog.Logger, _ string, _ ...string) (string, error) {
		calls++
		return "", nil
	})
	if err := task.Do(t.Context()); err == nil {
		t.Fatal("cleanup succeeded despite unit inspection failure")
	}
	if calls != 0 {
		t.Errorf("systemctl calls = %d, want 0", calls)
	}
	if got := readNPDFile(t, config); got != "config" {
		t.Errorf("config changed: %q", got)
	}
}

func TestRemoveHostPreservesArtifactsWhenDisableFails(t *testing.T) {
	t.Parallel()

	root := none.Root{HostRoot: t.TempDir()}
	unit := renderHostUnit(t, root)
	unitContent := readNPDFile(t, unit)
	binary := root.Path(npdBinaryPath)
	config := root.Path(npdConfigPath)
	writeNPDFile(t, binary, "binary")
	writeNPDFile(t, config, "config")

	disableErr := errors.New("disable failed")
	calls := 0
	task := newTestRemoveHost(t, root, func(_ context.Context, _ *slog.Logger, command string, args ...string) (string, error) {
		calls++
		checkDisableCommand(t, command, args)
		return "", disableErr
	})
	if err := task.Do(t.Context()); !errors.Is(err, disableErr) {
		t.Fatalf("cleanup error = %v, want disable failure", err)
	}
	if calls != 1 {
		t.Fatalf("disable calls = %d, want 1", calls)
	}
	for path, want := range map[string]string{unit: unitContent, binary: "binary", config: "config"} {
		if got := readNPDFile(t, path); got != want {
			t.Errorf("artifact %s changed: %q", path, got)
		}
	}
}

func TestRemoveHostRetriesPartialRemovalWithFreshTask(t *testing.T) {
	t.Parallel()

	root := none.Root{HostRoot: t.TempDir()}
	unit := renderHostUnit(t, root)
	unitContent := readNPDFile(t, unit)
	binary := root.Path(npdBinaryPath)
	config := root.Path(npdConfigPath)
	writeNPDFile(t, binary, "binary")
	writeNPDFile(t, filepath.Join(config, "blocker"), "blocker")

	calls := 0
	run := func(_ context.Context, _ *slog.Logger, command string, args ...string) (string, error) {
		calls++
		checkDisableCommand(t, command, args)
		if got := readNPDFile(t, unit); got != unitContent {
			t.Error("unit changed before disable")
		}
		return "", nil
	}
	if err := newTestRemoveHost(t, root, run).Do(t.Context()); err == nil || !strings.Contains(err.Error(), config) {
		t.Fatalf("cleanup error = %v, want removal failure for %s", err, config)
	}
	assertNPDAbsent(t, binary)
	if got := readNPDFile(t, unit); got != unitContent {
		t.Fatal("unit changed after partial removal")
	}
	if got := readNPDFile(t, filepath.Join(config, "blocker")); got != "blocker" {
		t.Fatalf("blocking file changed: %q", got)
	}

	if err := os.Remove(filepath.Join(config, "blocker")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(config); err != nil {
		t.Fatal(err)
	}
	writeNPDFile(t, config, "config")

	if err := newTestRemoveHost(t, root, run).Do(t.Context()); err != nil {
		t.Fatalf("fresh task retry: %v", err)
	}
	if calls != 2 {
		t.Fatalf("disable calls = %d, want 2", calls)
	}
	assertNPDAbsent(t, unit, binary, config)
}
