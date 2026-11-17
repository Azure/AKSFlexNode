package npd

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

type offlineNPDSystemd struct {
	t               *testing.T
	active, enabled bool
	calls           []string
	failAction      string
	failure         error
	afterAction     func(string)
}

func (s *offlineNPDSystemd) run(ctx context.Context, _ *slog.Logger, args ...string) (string, error) {
	s.t.Helper()
	s.calls = append(s.calls, strings.Join(args, " "))
	if len(args) < 2 || args[0] != "systemctl" {
		s.t.Fatalf("unexpected command: %v", args)
	}
	action := args[1]
	if action != "daemon-reload" && args[len(args)-1] != HostUnitNPD {
		s.t.Fatalf("command targets an unowned unit: %v", args)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if action == s.failAction {
		return "", s.failure
	}
	switch action {
	case "is-active":
		if !s.active {
			return "inactive", errors.New("inactive")
		}
	case "enable":
		s.active, s.enabled = true, true
	case "restart":
		s.active = true
	case "stop":
		s.active = false
	case "disable":
		s.enabled = false
	case "daemon-reload":
	default:
		s.t.Fatalf("unexpected systemctl action: %v", args)
	}
	if s.afterAction != nil {
		s.afterAction(action)
	}
	return "", nil
}

func offlineTransitionConfig() *config.Config {
	return &config.Config{Bootstrap: config.BootstrapConfig{
		OfflineArtifacts: config.OfflineArtifactsConfig{Source: "https://offline-user:offline-secret@example.invalid/artifacts?sig=offline-secret"},
	}}
}

func newOfflineTransitionHost(t *testing.T) (*goalstates.NodeStart, *offlineNPDSystemd, none.Root) {
	t.Helper()
	root := none.Root{HostRoot: t.TempDir(), Layout: none.Layout{Base: t.TempDir()}, Version: "online"}
	systemd := &offlineNPDSystemd{t: t}
	ns := &goalstates.NodeStart{RootOverride: root, NodeName: "flex-worker", ExecOverride: systemd.run}
	return ns, systemd, root
}

func startOfflineTransitionHost(t *testing.T, ns *goalstates.NodeStart, systemd *offlineNPDSystemd) []byte {
	t.Helper()
	task, ok := Start(slog.New(slog.DiscardHandler), &config.Config{}, ns.Root(), ns, StartOptions{
		UnitName: HostUnitNPD, BinaryPath: ns.Root().Path(BinaryPath),
	}).(*startTask)
	if !ok {
		t.Fatal("online Start did not return *startTask")
	}
	if err := task.Do(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !systemd.active || !systemd.enabled {
		t.Fatal("online NPD was not started and enabled")
	}
	content, err := os.ReadFile(ns.Root().Path("/etc/systemd/system/" + HostUnitNPD))
	if err != nil {
		t.Fatal(err)
	}
	systemd.calls = nil
	return content
}

func assertOfflineNPDUnit(t *testing.T, ns *goalstates.NodeStart, want []byte) {
	t.Helper()
	got, err := os.ReadFile(ns.Root().Path("/etc/systemd/system/" + HostUnitNPD))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("owned unit was changed or lost: %v", err)
	}
}

func TestHostNPDOnlineToOfflineTransition(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"start", "restart"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			ns, systemd, root := newOfflineTransitionHost(t)
			for _, version := range []string{"online", "offline"} {
				if _, err := root.Layout.Ensure(version); err != nil {
					t.Fatal(err)
				}
			}
			binary := root.Path(BinaryPath)
			if err := os.WriteFile(binary, []byte("online NPD"), 0o755); err != nil {
				t.Fatal(err)
			}
			if _, err := root.Layout.Flip("online"); err != nil {
				t.Fatal(err)
			}
			unit := startOfflineTransitionHost(t, ns, systemd)
			if _, err := root.Layout.Flip("offline"); err != nil {
				t.Fatal(err)
			}
			root.Version = "offline"
			ns.RootOverride = root
			if _, err := os.Stat(filepath.Join(root.Layout.BinDir(), "node-problem-detector")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("offline current unexpectedly contains NPD: %v", err)
			}

			var logs bytes.Buffer
			log := slog.New(slog.NewTextHandler(&logs, nil))
			cfg := offlineTransitionConfig()
			if err := Download(log, cfg, root).Do(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(systemd.calls) != 0 {
				t.Fatalf("offline download ran commands: %v", systemd.calls)
			}
			for range 2 {
				systemd.calls = nil
				task := Start(log, cfg, root, ns, StartOptions{
					UnitName: HostUnitNPD, BinaryPath: root.Path(BinaryPath), ForceRestart: operation == "restart",
				})
				if task.Name() != "start-npd" {
					t.Fatalf("offline task name = %q", task.Name())
				}
				if err := task.Do(t.Context()); err != nil {
					t.Fatal(err)
				}
				want := []string{"systemctl stop " + HostUnitNPD, "systemctl disable " + HostUnitNPD}
				if !slices.Equal(systemd.calls, want) || systemd.active || systemd.enabled {
					t.Fatalf("offline NPD: calls=%v active=%t enabled=%t", systemd.calls, systemd.active, systemd.enabled)
				}
				assertOfflineNPDUnit(t, ns, unit)
			}
			if strings.Contains(logs.String(), "offline-secret") || strings.Contains(logs.String(), "offline-user") {
				t.Fatal("offline NPD logged artifact credentials")
			}
		})
	}
}

func TestOfflineHostNPDFailureRetainsOwnershipAndRetries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, action string
		failure      error
	}{
		{name: "failed stop", action: "stop", failure: errors.New("stop failed")},
		{name: "failed disable", action: "disable", failure: errors.New("disable failed")},
		{name: "executor canceled", action: "stop", failure: context.Canceled},
		{name: "executor deadline", action: "disable", failure: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ns, systemd, _ := newOfflineTransitionHost(t)
			unit := startOfflineTransitionHost(t, ns, systemd)
			systemd.failAction, systemd.failure = tc.action, tc.failure
			log, cfg := slog.New(slog.DiscardHandler), offlineTransitionConfig()
			task := Start(log, cfg, ns.Root(), ns, StartOptions{
				UnitName: HostUnitNPD, BinaryPath: ns.Root().Path(BinaryPath), ForceRestart: true,
			})
			if err := task.Do(t.Context()); !errors.Is(err, tc.failure) {
				t.Fatalf("offline %s error = %v, want wrapped %v", tc.action, err, tc.failure)
			}
			want := []string{"systemctl stop " + HostUnitNPD}
			if tc.action == "disable" {
				want = append(want, "systemctl disable "+HostUnitNPD)
			}
			if !slices.Equal(systemd.calls, want) || !systemd.enabled || systemd.active != (tc.action == "stop") {
				t.Fatalf("failed cleanup: calls=%v active=%t enabled=%t", systemd.calls, systemd.active, systemd.enabled)
			}
			assertOfflineNPDUnit(t, ns, unit)
			systemd.failAction = ""
			for range 2 {
				systemd.calls = nil
				if err := task.Do(t.Context()); err != nil {
					t.Fatal(err)
				}
				if len(systemd.calls) != 2 || systemd.active || systemd.enabled {
					t.Fatalf("cleanup retry failed: calls=%v active=%t enabled=%t", systemd.calls, systemd.active, systemd.enabled)
				}
				assertOfflineNPDUnit(t, ns, unit)
			}
		})
	}
}

func TestOfflineHostNPDCancellationRetainsOwnership(t *testing.T) {
	t.Parallel()
	for _, after := range []string{"before cleanup", "stop", "disable"} {
		t.Run(after, func(t *testing.T) {
			t.Parallel()
			ns, systemd, _ := newOfflineTransitionHost(t)
			unit := startOfflineTransitionHost(t, ns, systemd)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if after == "before cleanup" {
				cancel()
			} else {
				systemd.afterAction = func(action string) {
					if action == after {
						cancel()
					}
				}
			}
			log, cfg := slog.New(slog.DiscardHandler), offlineTransitionConfig()
			if err := Start(log, cfg, ns.Root(), ns, StartOptions{
				UnitName: HostUnitNPD, BinaryPath: ns.Root().Path(BinaryPath), ForceRestart: true,
			}).Do(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled cleanup = %v", err)
			}
			var want []string
			if after != "before cleanup" {
				want = append(want, "systemctl stop "+HostUnitNPD)
			}
			if after == "disable" {
				want = append(want, "systemctl disable "+HostUnitNPD)
			}
			if !slices.Equal(systemd.calls, want) {
				t.Fatalf("commands after cancellation: got %v, want %v", systemd.calls, want)
			}
			assertOfflineNPDUnit(t, ns, unit)
			systemd.afterAction = nil
			if err := Start(log, cfg, ns.Root(), ns, StartOptions{
				UnitName: HostUnitNPD, BinaryPath: ns.Root().Path(BinaryPath), ForceRestart: true,
			}).Do(t.Context()); err != nil {
				t.Fatal(err)
			}
			if systemd.active || systemd.enabled {
				t.Fatal("canceled cleanup could not be retried")
			}
		})
	}
}

func TestOfflineNPDWithoutOwnedHostUnitDoesNotMutate(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"fresh host", "nspawn", "container"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			ns, systemd, _ := newOfflineTransitionHost(t)
			options := StartOptions{UnitName: HostUnitNPD, BinaryPath: ns.Root().Path(BinaryPath)}
			switch mode {
			case "nspawn":
				ns.RootOverride, ns.MachineDir = nil, t.TempDir()
				options = StartOptions{UnitName: "missing-npd.service"}
			case "container":
				ns.RootOverride = container.Root{SlotName: "kube1", Dir: t.TempDir(), HostRoot: t.TempDir()}
				ns.InContainer = true
				options = StartOptions{UnitName: "missing-npd.service"}
			}
			unitDir := ns.Root().Path("/etc/systemd/system")
			if err := os.MkdirAll(unitDir, 0o755); err != nil {
				t.Fatal(err)
			}
			unrelated := filepath.Join(unitDir, systemdUnitNPD)
			if err := os.WriteFile(unrelated, []byte("unrelated NPD unit"), 0o644); err != nil {
				t.Fatal(err)
			}
			unit := []byte("existing private-root host-named unit")
			if mode != "fresh host" {
				systemd.active, systemd.enabled = true, true
				if err := os.WriteFile(filepath.Join(unitDir, HostUnitNPD), unit, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			log, cfg := slog.New(slog.DiscardHandler), offlineTransitionConfig()
			for range 2 {
				options.ForceRestart = false
				if err := Start(log, cfg, ns.Root(), ns, options).Do(t.Context()); err != nil {
					t.Fatal(err)
				}
				options.ForceRestart = true
				if err := Start(log, cfg, ns.Root(), ns, options).Do(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if len(systemd.calls) != 0 {
				t.Fatalf("offline %s ran commands: %v", mode, systemd.calls)
			}
			if mode == "fresh host" {
				if _, err := os.Stat(filepath.Join(unitDir, HostUnitNPD)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("fresh offline host installed NPD: %v", err)
				}
			} else {
				assertOfflineNPDUnit(t, ns, unit)
				if !systemd.active || !systemd.enabled {
					t.Fatal("offline private-root NPD state was mutated")
				}
			}
			if got, err := os.ReadFile(unrelated); err != nil || string(got) != "unrelated NPD unit" {
				t.Fatalf("unrelated NPD unit was mutated: %v", err)
			}
		})
	}
}

func TestOfflineHostNPDPropagatesOwnershipStatError(t *testing.T) {
	t.Parallel()
	ns, systemd, _ := newOfflineTransitionHost(t)
	unit := ns.Root().Path("/etc/systemd/system/" + HostUnitNPD)
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(unit, unit); err != nil {
		t.Fatal(err)
	}
	_, statErr := os.Stat(unit)
	var pathErr *os.PathError
	if !errors.As(statErr, &pathErr) {
		t.Fatalf("expected unit stat failure, got %v", statErr)
	}
	if err := Start(slog.New(slog.DiscardHandler), offlineTransitionConfig(), ns.Root(), ns, StartOptions{
		UnitName: HostUnitNPD, BinaryPath: ns.Root().Path(BinaryPath), ForceRestart: true,
	}).Do(t.Context()); !errors.Is(err, pathErr.Err) {
		t.Fatalf("ownership stat error was hidden: %v", err)
	}
	if len(systemd.calls) != 0 {
		t.Fatalf("commands ran without verifying ownership: %v", systemd.calls)
	}
}
