package noneops

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/daemon/internal/hostnode"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

type fakeHost struct {
	mu     sync.Mutex
	active map[string]bool
	fail   map[string]error
	calls  []string
}

func (h *fakeHost) run(_ context.Context, _ *slog.Logger, name string, args ...string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	call := name + " " + strings.Join(args, " ")
	h.calls = append(h.calls, call)
	if err := h.fail[call]; err != nil {
		return "", err
	}
	if name == "systemctl" && len(args) > 1 {
		unit := args[len(args)-1]
		switch args[0] {
		case "enable", "restart":
			h.active[unit] = true
		case "stop":
			delete(h.active, unit)
		case "is-active":
			if !h.active[unit] {
				return "inactive", errors.New("inactive")
			}
			return "active", nil
		}
	}
	return "", nil
}

type nodeFixture struct {
	node        *Node
	host        *fakeHost
	root, units string
}

func newNodeFixture(t *testing.T, version string) nodeFixture {
	t.Helper()
	root, units, base := t.TempDir(), t.TempDir(), t.TempDir()
	h := &fakeHost{active: map[string]bool{}, fail: map[string]error{}}
	cfg := &agentconfig.AgentConfig{Isolation: "none"}
	cfg.Cluster.Version = version
	gs := &goalstates.MachineGoalState{
		RootFS: &goalstates.RootFS{HostArch: "amd64", KubernetesVersion: version, ContainerdVersion: "2.1.8", RunCVersion: "1.5.0", CNIPluginVersion: "1.5.1"},
		NodeStart: &goalstates.NodeStart{
			MachineName: "kube1", NodeName: "worker-1",
			Containerd: goalstates.Containerd{SandboxImage: "pause", CNIBinDir: "/opt/cni/bin", CNIConfDir: "/etc/cni/net.d"},
			Gantry:     goalstates.Gantry{Disabled: true},
			Kubelet:    goalstates.Kubelet{APIServer: "https://api", CACertData: []byte("ca"), ClusterDNS: "10.0.0.10", KubeletAuthInfo: agentconfig.KubeletAuthInfo{BootstrapToken: "token"}},
		},
	}
	log := slog.New(slog.DiscardHandler)
	p := Params{
		Log: log, Config: cfg, Generation: "42", GoalState: gs,
		Overrides: Overrides{
			Layout: none.Layout{Base: filepath.Join(base, "node")}, SystemdDir: units, Run: h.run,
			Proc: filepath.Join(root, "proc"), ImageArchiveDir: filepath.Join(root, "images"),
			Download:    func(context.Context) error { return nil },
			WaitKubelet: func(context.Context) error { return nil },
		},
		Hooks: hostnode.Hooks{HostRoot: root,
			Resolve: func(string) (*goalstates.MachineGoalState, error) {
				rf, ns := *gs.RootFS, *gs.NodeStart
				return &goalstates.MachineGoalState{RootFS: &rf, NodeStart: &ns}, nil
			},
			SetupNetwork:   func(context.Context) error { return nil },
			CleanupNetwork: func(context.Context) error { return nil },
		},
	}
	return nodeFixture{node: New(p), host: h, root: root, units: units}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertExists(t *testing.T, path string, want bool) {
	t.Helper()
	_, err := os.Stat(path)
	if want && err != nil {
		t.Fatalf("expected %s: %v", path, err)
	}
	if !want && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected %s absent, got %v", path, err)
	}
}

func installOld(t *testing.T, f nodeFixture) string {
	t.Helper()
	version := "1.33.0-previous"
	if _, err := f.node.p.Layout.Ensure(version); err != nil {
		t.Fatal(err)
	}
	if _, err := f.node.p.Layout.Flip(version); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(f.root, none.ContainerdConfigPath), "old config")
	return version
}

func TestProvisionAndRestart(t *testing.T) {
	t.Parallel()
	f := newNodeFixture(t, "1.34.1")
	for range 2 {
		if err := f.node.Provision(t.Context()); err != nil {
			t.Fatal(err)
		}
		cd, kl, err := f.node.Configure(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := f.node.Start(t.Context(), false, cd, kl); err != nil {
			t.Fatal(err)
		}
	}
	gs, err := f.node.GoalState()
	if err != nil {
		t.Fatal(err)
	}
	if !gs.NodeStart.HostNative() || gs.NodeStart.Root().Path("/etc/cni") != filepath.Join(f.root, "etc/cni") {
		t.Fatal("node phases do not use the host-native root")
	}
	assertExists(t, filepath.Join(f.units, none.UnitKubelet), true)
	if err := f.node.Restart(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, call := range f.host.calls {
		if strings.Contains(call, "machinectl") || strings.Contains(call, "systemd-run") || strings.Contains(call, "reboot") {
			t.Fatalf("host-native lifecycle ran %s", call)
		}
	}
}

func TestUpgradeAndRecovery(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		failStart bool
	}{
		{name: "commit healthy target"}, {name: "retain failed rollback journal", failStart: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newNodeFixture(t, "1.34.1")
			old := installOld(t, f)
			if tc.failStart {
				f.host.fail["systemctl enable --now "+none.UnitContainerd] = errors.New("containerd start failed")
			}
			err := f.node.Upgrade(t.Context())
			if (err != nil) != tc.failStart {
				t.Fatalf("Upgrade error = %v", err)
			}
			assertExists(t, f.node.journalPath(), true)
			assertExists(t, f.node.configSnapshotDir(), true)
			if tc.failStart {
				if f.host.active[none.UnitContainerd] {
					t.Fatal("rollback should have failed")
				}
				delete(f.host.fail, "systemctl enable --now "+none.UnitContainerd)
				// Recovery runs from the old applied configuration, not the failed target.
				f.node.p.Config.Cluster.Version = "1.33.0"
				f.node.p.Generation = "41"
			}
			if err := f.node.RecoverUpgrade(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := f.node.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := f.node.ClearStartup(); err != nil {
				t.Fatal(err)
			}
			assertExists(t, f.node.journalPath(), false)
			assertExists(t, f.node.configSnapshotDir(), false)
			cur, err := f.node.p.Layout.Current()
			if err != nil {
				t.Fatal(err)
			}
			if tc.failStart && cur != old {
				t.Fatalf("active version = %s, want %s", cur, old)
			}
			if !f.host.active[none.UnitContainerd] || !f.host.active[none.UnitKubelet] {
				t.Fatal("recovery left the stack stopped")
			}
			if err := f.node.RecoverUpgrade(t.Context()); err != nil {
				t.Fatalf("repeated recovery: %v", err)
			}
		})
	}
}

func TestConfigOnlyRecoveryUsesAppliedDigest(t *testing.T) {
	t.Parallel()
	f := newNodeFixture(t, "1.34.1")
	if err := f.node.Provision(t.Context()); err != nil {
		t.Fatal(err)
	}
	version, err := f.node.Version()
	if err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(f.root, none.ContainerdConfigPath)
	mustWrite(t, oldPath, "old config")
	if _, err := hostnode.TakeSnapshot(f.node.p.Log, f.node.configSnapshotDir(), f.root); err != nil {
		t.Fatal(err)
	}
	if err := f.node.writeJournal(upgradeJournal{Previous: version, Target: version, TargetConfig: "different-config", TargetGeneration: f.node.p.Generation}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, oldPath, "new config")
	if err := f.node.RecoverUpgrade(t.Context()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(oldPath)
	if err != nil || string(data) != "old config" {
		t.Fatalf("config after rollback = %q, %v", data, err)
	}
	if !f.host.active[none.UnitKubelet] {
		t.Fatal("config-only recovery did not restart kubelet")
	}
}

func TestTeardownPreservesLiveDataOnFailure(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"unit stop", "pod reaping", "volume unmount"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()
			f := newNodeFixture(t, "1.34.1")
			if err := f.node.Provision(t.Context()); err != nil {
				t.Fatal(err)
			}
			mountInfo := filepath.Join(f.root, "mountinfo")
			mustWrite(t, mountInfo, "1 0 8:1 / / rw - ext4 /dev/sda1 rw\n")
			td := &Teardown{Log: f.node.p.Log, Root: f.root, Layout: f.node.p.Layout, SystemdDir: f.units, Run: f.host.run, Proc: filepath.Join(f.root, "proc"), MountInfo: mountInfo}
			kubeletData := filepath.Join(f.root, "var/lib/kubelet/pods/live/data")
			mustWrite(t, kubeletData, "preserve")
			switch failure {
			case "unit stop":
				f.host.fail["systemctl stop "+none.UnitKubelet] = errors.New("stuck")
			case "pod reaping":
				td.CgroupRoot = filepath.Join(f.root, "cgroup")
				if err := os.MkdirAll(filepath.Join(td.CgroupRoot, "kubepods.slice/cgroup.kill"), 0o750); err != nil {
					t.Fatal(err)
				}
			case "volume unmount":
				mustWrite(t, mountInfo, "2 1 0:30 / /var/lib/kubelet/pods/live rw - nfs srv:/volume rw\n")
				f.host.fail["umount -l /var/lib/kubelet/pods/live"] = errors.New("busy")
			}
			if err := td.Destroy().Do(t.Context()); err == nil {
				t.Fatal("expected teardown failure")
			}
			assertExists(t, kubeletData, true)
			if !td.Present() {
				t.Fatal("failed reset must remain discoverable for retry")
			}
			// Clear the fault and retry; the marker must select teardown even if
			// the backend already removed its own units and layout.
			clear(f.host.fail)
			if td.CgroupRoot != "" {
				if err := os.RemoveAll(td.CgroupRoot); err != nil {
					t.Fatal(err)
				}
			}
			if err := td.Destroy().Do(t.Context()); err != nil {
				t.Fatalf("reset retry: %v", err)
			}
			assertExists(t, kubeletData, false)
			if err := td.Destroy().Do(t.Context()); err != nil {
				t.Fatalf("repeated reset: %v", err)
			}
		})
	}
}
