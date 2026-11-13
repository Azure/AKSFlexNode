package noneops

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/daemon/internal/hostnode"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

func TestExposedStartupNeverRollsBack(t *testing.T) {
	t.Parallel()
	for _, failEnable := range []bool{false, true} {
		t.Run(map[bool]string{false: "health failure", true: "kubelet start failure"}[failEnable], func(t *testing.T) {
			t.Parallel()
			f := newNodeFixture(t, "1.34.1")
			old := installOld(t, f)
			reaps, cleanups := 0, 0
			f.node.p.ReapPods = func() error { reaps++; return nil }
			f.node.p.CleanupNetwork = func(context.Context) error { cleanups++; return nil }
			f.node.p.WaitKubelet = func(context.Context) error { return errors.New("not healthy yet") }
			f.node.p.Run = func(ctx context.Context, log *slog.Logger, name string, args ...string) (string, error) {
				if name == "systemctl" && len(args) > 1 && args[0] == "enable" && args[len(args)-1] == none.UnitKubelet {
					s, err := f.node.PendingStartup()
					if err != nil || s == nil || s.Restored {
						t.Fatalf("kubelet exposed without target intent: %v", err)
					}
					if failEnable {
						return "", errors.New("kubelet enable interrupted")
					}
				}
				return f.host.run(ctx, log, name, args...)
			}
			if err := f.node.Upgrade(t.Context()); err == nil {
				t.Fatal("expected startup error")
			}
			current, err := f.node.p.Layout.Current()
			if err != nil || current == old {
				t.Fatal("exposed target was rolled back")
			}
			beforeReaps, beforeCleanups := reaps, cleanups
			beforeCalls := len(f.host.calls)
			p := f.node.p
			p.Run, p.WaitKubelet = f.host.run, func(context.Context) error { return nil }
			// Simulate a process crash immediately after the boundary, including
			// the case where kubelet has not actually started yet.
			for range 2 {
				if err := New(p).RecoverUpgrade(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if reaps != beforeReaps || cleanups != beforeCleanups {
				t.Fatal("completion repeated destructive preparation")
			}
			for _, call := range f.host.calls[beforeCalls:] {
				if strings.Contains(call, "systemctl stop") || strings.Contains(call, "systemctl restart") {
					t.Fatalf("completion disturbed running services: %s", call)
				}
			}
			data, err := os.ReadFile(filepath.Join(f.root, none.ContainerdConfigPath))
			if err != nil || string(data) == "old config" {
				t.Fatal("completion restored the old configuration")
			}
			if !f.host.active[none.UnitKubelet] {
				t.Fatal("missing kubelet was not started")
			}
		})
	}
}

func TestSourceRestorationClearsCNIAndKeepsCredentials(t *testing.T) {
	t.Parallel()
	f := newNodeFixture(t, "1.34.1")
	installOld(t, f)
	conf := filepath.Join(f.root, goalstates.CNIConfigDir)
	ipam := filepath.Join(f.root, hostnode.CNIStateDir)
	credentials := filepath.Join(f.root, goalstates.KubeletKubeconfigPath)
	clientKey := filepath.Join(f.root, hostnode.KubeletClientPKIDir, "client.pem")
	mustWrite(t, filepath.Join(conf, "10-old.conflist"), "previous pod CIDR")
	mustWrite(t, filepath.Join(ipam, "old-network", "10.1.0.2"), "reaped pod reservation")
	mustWrite(t, credentials, "rotated source credentials")
	mustWrite(t, clientKey, "source key fixture")
	f.node.p.Stage = func(context.Context) error { return errors.New("artifact staging interrupted") }
	if err := f.node.Upgrade(t.Context()); err == nil {
		t.Fatal("expected preparation failure")
	}
	entries, err := os.ReadDir(conf)
	if err != nil || len(entries) != 0 {
		t.Fatalf("restored source retained old CNI config: %v", err)
	}
	assertExists(t, ipam, false)
	for path, want := range map[string]string{credentials: "rotated source credentials", clientKey: "source key fixture"} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != want {
			t.Fatalf("source credentials %s were not preserved: %v", path, err)
		}
	}
	s, err := f.node.PendingStartup()
	if err != nil || s == nil || !s.Restored {
		t.Fatalf("missing restored-source startup: %v", err)
	}
}

func TestInitialStopFailureDoesNotPublishSourceRegistration(t *testing.T) {
	t.Parallel()
	f := newNodeFixture(t, "1.34.1")
	old := installOld(t, f)
	f.host.active[none.UnitKubelet], f.host.active[none.UnitContainerd] = true, true
	f.host.fail["systemctl stop "+none.UnitKubelet] = errors.New("source kubelet did not stop")
	reaps := 0
	f.node.p.ReapPods = func() error { reaps++; return nil }
	if err := f.node.Upgrade(t.Context()); err == nil {
		t.Fatal("expected failed source stop")
	}
	if s, err := f.node.PendingStartup(); err != nil || s != nil {
		t.Fatalf("a still-active source acquired a fresh startup intent: %v", err)
	}
	if reaps != 0 || !f.host.active[none.UnitKubelet] {
		t.Fatal("failed stop caused pod cleanup or hid the still-active kubelet")
	}
	assertExists(t, f.node.journalPath(), true)
	for _, call := range f.host.calls {
		if strings.Contains(call, "systemctl enable") || strings.Contains(call, "systemctl restart") {
			t.Fatalf("failed stop invented another source registration: %s", call)
		}
	}
	// After another RP drain/delete, a confirmed full stop may restore and
	// publish source startup. Recovery while a Node exists is tested above
	// the runtime at the reconciler boundary.
	delete(f.host.fail, "systemctl stop "+none.UnitKubelet)
	if err := New(f.node.p).RecoverUpgrade(t.Context()); err != nil {
		t.Fatal(err)
	}
	s, err := f.node.PendingStartup()
	if err != nil || s == nil || !s.Restored || s.Version != old || reaps == 0 {
		t.Fatalf("authorized recovery did not prepare a confirmed source start: %v", err)
	}
}

func TestRestoredSourceStartupIsForwardOnly(t *testing.T) {
	t.Parallel()
	f := newNodeFixture(t, "1.34.1")
	old := installOld(t, f)
	f.node.p.Previous = f.node.p.Config.DeepCopy()
	f.node.p.PreviousGeneration = "41"
	reaps := 0
	f.node.p.ReapPods = func() error { reaps++; return nil }
	f.node.p.Run = func(ctx context.Context, log *slog.Logger, name string, args ...string) (string, error) {
		if name == "systemctl" && len(args) > 1 && args[0] == "enable" {
			current, err := f.node.p.Layout.Current()
			if err != nil {
				return "", err
			}
			unit := args[len(args)-1]
			if current != old && unit == none.UnitContainerd {
				return "", errors.New("target containerd failed before exposure")
			}
			if current == old && unit == none.UnitKubelet {
				s, err := f.node.PendingStartup()
				if err != nil || s == nil || !s.Restored || s.Generation != "41" {
					t.Fatalf("source exposed without source intent: %v", err)
				}
				return "", errors.New("source start interrupted")
			}
		}
		return f.host.run(ctx, log, name, args...)
	}
	if err := f.node.Upgrade(t.Context()); err == nil {
		t.Fatal("expected target failure")
	}
	before, beforeCalls := reaps, len(f.host.calls)
	p := f.node.p
	p.Run = f.host.run
	if err := New(p).RecoverUpgrade(t.Context()); err != nil {
		t.Fatal(err)
	}
	if reaps != before {
		t.Fatal("source recovery reaped newly admitted workloads")
	}
	for _, call := range f.host.calls[beforeCalls:] {
		if strings.Contains(call, "systemctl stop") || strings.Contains(call, "systemctl restart") {
			t.Fatalf("source recovery disturbed services: %s", call)
		}
	}
}
