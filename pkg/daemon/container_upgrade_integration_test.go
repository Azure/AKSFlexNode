package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/containerops"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostnode"
	"github.com/Azure/unbounded/pkg/agent/hostnode/hostnodetest"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Only artifact I/O, host commands and networking are replaced. Prepare,
// Configure, Start, Upgrade and rollback execute the SDK's original recipe.
type containerSDKFixture struct {
	t       *testing.T
	base    string
	mu      sync.Mutex
	active  map[string]bool
	events  []string
	fail    string
	failAll map[string]error
	boom    error
	onEvent func(string)
}

func newContainerSDKFixture(t *testing.T) *containerSDKFixture {
	t.Helper()
	f := &containerSDKFixture{t: t, base: t.TempDir(), active: make(map[string]bool), boom: errors.New("injected SDK failure")}
	if err := os.WriteFile(filepath.Join(f.base, "mountinfo"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *containerSDKFixture) event(event string) error {
	f.events = append(f.events, event)
	if f.onEvent != nil {
		f.onEvent(event)
	}
	if f.fail == event {
		f.fail = ""
		return f.boom
	}
	return f.failAll[event]
}

func (f *containerSDKFixture) run(ctx context.Context, _ *slog.Logger, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := f.event(name + " " + strings.Join(args, " ")); err != nil {
		return "", err
	}
	if name == "systemctl" {
		unit := args[len(args)-1]
		switch args[0] {
		case "enable", "restart":
			f.active[unit] = true
		case "stop", "disable":
			delete(f.active, unit)
		case "is-active":
			if !f.active[unit] {
				return "", errors.New("inactive")
			}
		}
	}
	if name == f.runtime() && (args[0] == "state" || args[0] == "exec") {
		slot := strings.TrimPrefix(args[1], "unbounded-")
		switch args[0] {
		case "state":
			if !f.active[container.UnitName(slot)] {
				return "", errors.New("container does not exist")
			}
			return `{"status":"running"}`, nil
		case "exec":
			if !f.active[container.UnitName(slot)] {
				return "", errors.New("container not running")
			}
			if strings.HasPrefix(strings.Join(args[2:], " "), "systemctl show") {
				return "running", nil
			}
			return "active", nil
		}
	}
	return "", nil
}

func (f *containerSDKFixture) runtime() string { return filepath.Join(f.base, "runtime/runc") }
func (f *containerSDKFixture) hostPath(path string) string {
	return filepath.Join(f.base, "host", path)
}
func (f *containerSDKFixture) rootPath(slot string) string {
	return filepath.Join(f.base, "containers", slot)
}
func (f *containerSDKFixture) snapshotPath() string { return filepath.Join(f.base, "snapshot") }

func (f *containerSDKFixture) node(p containerops.Params) managedContainerNode {
	if p.GoalState == nil {
		p.GoalState = hostnodetest.GoalState(p.Slot, false)
	}
	p.Overrides = containerops.Overrides{
		Layout:     container.Layout{Base: filepath.Join(f.base, "containers")},
		SystemdDir: filepath.Join(f.base, "systemd"), RuntimePath: f.runtime(),
		HelperPath: filepath.Join(f.base, "agent/aks-flex-node"), Run: f.run,
		MountInfoPath: filepath.Join(f.base, "mountinfo"), Host: &container.HostFS{SELinuxEnforce: filepath.Join(f.base, "absent")},
		PollInterval: time.Millisecond, GoneTimeout: 20 * time.Millisecond, ReadyTimeout: 50 * time.Millisecond,
		ImageArchiveDir: filepath.Join(f.base, "images"),
		Unpack: func(_ context.Context, dir string) error {
			if err := f.event("unpack-" + p.Slot); err != nil {
				return err
			}
			return os.MkdirAll(filepath.Join(dir, "etc"), 0o750)
		},
		InstallRuntime: func(context.Context) error { return nil },
		InstallHelper:  func(context.Context) error { return nil },
		ProvisionRootfs: func(context.Context) error {
			if err := f.event("provision-" + p.Slot); err != nil {
				return err
			}
			path := f.hostPath("opt/cni/bin/test-plugin")
			if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
				return err
			}
			return os.WriteFile(path, []byte(p.Slot), 0o600)
		},
	}
	p.Hooks = hostnode.Hooks{
		HostRoot: f.hostPath(""), ConfigSnapshotDir: f.snapshotPath(),
		SetupNetwork:   func(context.Context) error { return f.event("setup-" + p.Slot) },
		CleanupNetwork: func(context.Context) error { return f.event("cleanup-" + p.Slot) },
		WaitLocalDNS:   func(context.Context, *goalstates.NodeStart) error { return nil },
	}
	return managedContainerNode{containerops.New(p)}
}

func (f *containerSDKFixture) startSource() managedContainerNode {
	f.t.Helper()
	node := f.node(containerops.Params{Log: discardLogger(), Slot: "kube1"})
	if _, err := node.Prepare(f.t.Context()); err != nil {
		f.t.Fatal(err)
	}
	cri, kubelet, err := node.Configure(f.t.Context())
	if err != nil {
		f.t.Fatal(err)
	}
	if err := node.Start(f.t.Context(), false, cri, kubelet); err != nil {
		f.t.Fatal(err)
	}
	for _, path := range []string{goalstates.KubeletKubeconfigPath, hostnode.KubeletClientPKIDir + "/kubelet-client-current.pem", goalstates.KubeletConfigurationPath} {
		writeTestFile(f.t, f.hostPath(path), "source")
	}
	f.events = nil
	return node
}

func requireContainerEventOrder(t *testing.T, events []string, want ...string) {
	t.Helper()
	at := 0
	for _, event := range want {
		i := slices.Index(events[at:], event)
		if i < 0 {
			t.Fatalf("missing %q after %d in %v", event, at, events)
		}
		at += i + 1
	}
}

func TestContainerOriginalSDKUpgradeRollback(t *testing.T) {
	t.Parallel()
	for _, step := range []string{"unpack", "artifacts", "provision", "stop", "cleanup", "configure", "container start", "kubelet start", "bootstrap", "readiness"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			f := newContainerSDKFixture(t)
			old := f.startSource()
			next := f.node(containerops.Params{Log: discardLogger(), Slot: "kube2"})
			if err := wrapContainerArtifactUnpack(next, func(context.Context, *goalstates.MachineGoalState) error {
				return f.event("artifacts-kube2")
			}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			f.onEvent = func(event string) {
				if (step == "bootstrap" || step == "readiness") && event == f.fail {
					cancel()
				}
				if event == "systemctl enable --now "+container.UnitName("kube1") {
					data, err := os.ReadFile(f.hostPath(goalstates.KubeletKubeconfigPath))
					if err != nil || string(data) != "source" {
						t.Fatalf("SDK resumed source before restoring identity: %q, %v", data, err)
					}
				}
			}
			f.fail = map[string]string{
				"unpack": "unpack-kube2", "artifacts": "artifacts-kube2", "provision": "provision-kube2",
				"stop": "systemctl stop " + container.UnitName("kube1"), "cleanup": "cleanup-kube2", "configure": "setup-kube2",
				"container start": "systemctl enable --now " + container.UnitName("kube2"),
				"kubelet start":   f.runtime() + " exec unbounded-kube2 systemctl enable --now kubelet.service",
				"bootstrap":       f.runtime() + " exec unbounded-kube2 test -s " + goalstates.KubeletKubeconfigPath,
				"readiness":       f.runtime() + " exec unbounded-kube2 systemctl is-active kubelet.service",
			}[step]
			wantErr := f.boom
			if step == "bootstrap" || step == "readiness" {
				wantErr = context.Canceled
			}
			err := next.Upgrade(ctx, old)
			if !errors.Is(err, wantErr) {
				t.Fatalf("original Upgrade lost %s failure: %v", step, err)
			}
			if !f.active[container.UnitName("kube1")] || f.active[container.UnitName("kube2")] {
				t.Fatalf("SDK did not restore source alone: %v", f.active)
			}
			for path, want := range map[string]string{
				goalstates.KubeletKubeconfigPath: "source", hostnode.KubeletClientPKIDir + "/kubelet-client-current.pem": "source",
				goalstates.KubeletConfigurationPath: "source", "/opt/cni/bin/test-plugin": "kube1",
			} {
				data, err := os.ReadFile(f.hostPath(path))
				if err != nil || string(data) != want {
					t.Errorf("SDK rollback shared %s = %q, %v; want %q", path, data, err, want)
				}
			}
			for _, path := range []string{f.rootPath("kube2"), f.snapshotPath()} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("SDK should remove %s after successful rollback: %v", path, err)
				}
			}
			if step == "unpack" && slices.Contains(f.events, "artifacts-kube2") {
				t.Fatal("Flex installed artifacts after failed image unpack")
			}
			if step == "artifacts" && slices.Contains(f.events, "provision-kube2") {
				t.Fatal("SDK provisioned after failed Flex artifact install")
			}
		})
	}
}

func TestContainerOriginalSDKUpgradeRefusesUnsafeStaleTargetCleanup(t *testing.T) {
	t.Parallel()
	f := newContainerSDKFixture(t)
	old := f.startSource()
	next := f.node(containerops.Params{Log: discardLogger(), Slot: "kube2"})
	writeTestFile(t, filepath.Join(f.rootPath("kube2"), "stale"), "old image")
	mount := "100 1 0:50 / " + f.rootPath("kube2") + "/var/lib/containerd rw - xfs /dev/sda rw\n"
	if err := os.WriteFile(filepath.Join(f.base, "mountinfo"), []byte(mount), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := next.Upgrade(t.Context(), old); err == nil || !strings.Contains(err.Error(), "remove stale slot kube2") {
		t.Fatalf("unsafe stale cleanup was accepted: %v", err)
	}
	if !f.active[container.UnitName("kube1")] || slices.Contains(f.events, "unpack-kube2") ||
		slices.Contains(f.events, "systemctl stop "+container.UnitName("kube1")) {
		t.Fatalf("stale cleanup failure touched source or prepared target: %v", f.events)
	}
}

func TestContainerOriginalSDKUpgradeDoesNotResumeBesideLiveTarget(t *testing.T) {
	t.Parallel()
	f := newContainerSDKFixture(t)
	old := f.startSource()
	next := f.node(containerops.Params{Log: discardLogger(), Slot: "kube2"})
	f.fail = f.runtime() + " exec unbounded-kube2 systemctl enable --now kubelet.service"
	f.failAll = map[string]error{
		"systemctl stop " + container.UnitName("kube2"):    errors.New("target stop stuck"),
		"systemctl disable " + container.UnitName("kube2"): errors.New("target disable stuck"),
	}
	err := next.Upgrade(t.Context(), old)
	if !errors.Is(err, f.boom) || !strings.Contains(err.Error(), "previous slot kube1 is left stopped") {
		t.Fatalf("SDK did not report failed rollback: %v", err)
	}
	if f.active[container.UnitName("kube1")] || !f.active[container.UnitName("kube2")] {
		t.Fatalf("unsafe active slots after failed rollback: %v", f.active)
	}
	for _, path := range []string{f.snapshotPath(), f.rootPath("kube1"), f.rootPath("kube2")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("SDK did not retain recovery data at %s: %v", path, err)
		}
	}
}

func TestContainerOriginalSDKUpgradeSuccessAndSlotRotation(t *testing.T) {
	t.Parallel()
	f := newContainerSDKFixture(t)
	old := f.startSource()
	for _, slot := range []string{"kube2", "kube1"} {
		next := f.node(containerops.Params{Log: discardLogger(), Slot: slot})
		stale := filepath.Join(f.rootPath(slot), "stale")
		writeTestFile(t, stale, "stale image")
		if err := wrapContainerArtifactUnpack(next, func(context.Context, *goalstates.MachineGoalState) error {
			return f.event("artifacts-" + slot)
		}); err != nil {
			t.Fatal(err)
		}
		if err := next.Upgrade(t.Context(), old); err != nil {
			t.Fatal(err)
		}
		if !f.active[container.UnitName(slot)] || f.active[container.UnitName(container.OtherSlot(slot))] {
			t.Fatalf("incorrect active slots: %v", f.active)
		}
		for _, path := range []string{stale, f.snapshotPath()} {
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("successful SDK Upgrade retained %s: %v", path, err)
			}
		}
		requireContainerEventOrder(t, f.events, "unpack-"+slot, "artifacts-"+slot, "provision-"+slot,
			"systemctl stop "+container.UnitName(container.OtherSlot(slot)), "systemctl enable --now "+container.UnitName(slot))
		if _, err := os.Stat(f.rootPath(container.OtherSlot(slot))); err != nil {
			t.Fatalf("SDK unexpectedly retired source: %v", err)
		}
		if err := old.Destroy(t.Context()); err != nil {
			t.Fatal(err)
		}
		old = next
		f.events = nil
	}
}

func TestContainerArtifactUnpackBeforeProvisionAndStart(t *testing.T) {
	t.Parallel()
	for _, offline := range []bool{false, true} {
		t.Run(fmt.Sprintf("offline=%t", offline), func(t *testing.T) {
			t.Parallel()
			f := newContainerSDKFixture(t)
			cfg := &config.Config{Bootstrap: config.BootstrapConfig{Isolation: "container", ExperimentalContainerIsolation: true},
				Azure: config.AzureConfig{ManagedIdentity: &config.ManagedIdentityConfig{ClientID: "msi-or-arc-client"}}}
			if offline {
				cfg.Bootstrap.OfflineArtifacts.Source = "file:///unused-offline-artifacts"
			}
			ac := config.ToAgentConfig(cfg, "kube1")
			if ac.Kubelet.Auth.ExecCredential == nil {
				t.Fatal("fixture did not select MSI/Arc exec authentication")
			}
			gs := hostnodetest.GoalState("kube1", false)
			gs.NodeStart.Kubelet.KubeletAuthInfo = ac.Kubelet.Auth
			node := f.node(containerops.Params{Log: discardLogger(), Slot: "kube1", GoalState: gs})
			if !offline {
				// The image already carries this NPD version; no network is needed.
				fixture, err := filepath.Abs("testdata/container_npd.sh")
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(f.rootPath("kube1"), "usr/local/bin/node-problem-detector")
				if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(fixture, path); err != nil {
					t.Fatal(err)
				}
			}
			if err := wrapContainerArtifactUnpack(node, containerArtifacts(cfg, discardLogger())); err != nil {
				t.Fatal(err)
			}
			checked := 0
			f.onEvent = func(event string) {
				if event != "provision-kube1" && event != "systemctl enable --now "+container.UnitName("kube1") {
					return
				}
				checked++
				helper := gs.RootFS.Root().Path(ac.Kubelet.Auth.ExecCredential.Command)
				if info, err := os.Stat(helper); err != nil || info.Mode().Perm()&0o111 == 0 {
					t.Fatalf("credential helper not installed before %s: %v", event, err)
				}
				npd := gs.RootFS.Root().Path("/usr/local/bin/node-problem-detector")
				_, err := os.Stat(npd)
				if (!offline && err != nil) || (offline && !errors.Is(err, os.ErrNotExist)) {
					t.Fatalf("NPD availability before %s (offline=%t): %v", event, offline, err)
				}
			}
			// A second prepare of a stopped slot exercises the wrapper's idempotency.
			for range 2 {
				if _, err := node.Prepare(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			cri, kubelet, err := node.Configure(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if err := node.Start(t.Context(), false, cri, kubelet); err != nil {
				t.Fatal(err)
			}
			if checked != 3 {
				t.Fatalf("artifact checks = %d, want two provisions and one start", checked)
			}
		})
	}
}

func TestContainerCompletionJournalWithOriginalSDK(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"healthy", "SDK", "NPD", "completion publication"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()
			f := newContainerSDKFixture(t)
			f.startSource()
			op, _, store := containerOperatorFixture(t, "", false)
			op.cfg.Bootstrap.OfflineArtifacts.Source = "file:///unused-offline-artifacts"
			op.newNode = func(p containerops.Params) containerNode { return f.node(p) }
			op.resolve = func(_ context.Context, _ *slog.Logger, cfg *config.Config, slot string) (*agentconfig.AgentConfig, *goalstates.MachineGoalState, *goalstates.ContainerImageArchiveStaging, error) {
				return config.ToAgentConfig(cfg, slot), hostnodetest.GoalState(slot, false), nil, nil
			}
			if failure == "SDK" {
				f.fail = f.runtime() + " exec unbounded-kube2 systemctl enable --now kubelet.service"
			}
			seenExposure := false
			f.onEvent = func(event string) {
				if event != "systemctl enable --now "+container.UnitName("kube2") || seenExposure {
					return
				}
				seenExposure = true
				if lock, err := agentbinary.AcquireHostActivationLock(op.activationLockPath); err == nil {
					if err := lock.Close(); err != nil {
						t.Fatal(err)
					}
					t.Fatal("SDK upgrade did not retain shared agent activation lock")
				}
				startup, err := op.startup.Load()
				if err != nil || startup == nil || startup.Phase != "preparing" {
					t.Fatalf("completion was published before SDK return: %+v, %v", startup, err)
				}
				if info, err := os.Stat(filepath.Join(f.rootPath("kube2"), "usr/local/bin/aks-flex-node")); err != nil || info.Mode().Perm()&0o111 == 0 {
					t.Fatalf("target exposed without Flex credential helper: %v", err)
				}
				if failure == "completion publication" {
					// Let the SDK succeed but prevent Flex from recording its return.
					if err := os.Rename(filepath.Dir(op.startup.path), filepath.Dir(op.startup.path)+"-saved"); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Dir(op.startup.path), nil, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			op.startNPD = func(context.Context, *slog.Logger, *config.Config, *goalstates.NodeStart) error {
				if _, err := os.Stat(f.snapshotPath()); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("SDK did not discard snapshot before Flex completion: %v", err)
				}
				if failure == "NPD" {
					return errors.New("NPD unavailable")
				}
				return nil
			}
			_, err := op.ApplyGoalState(t.Context(), discardLogger(), aksmachine.GoalState{SettingsVersion: "new", KubernetesVersion: "1.36.2"})
			if (err != nil) != (failure != "healthy") || !seenExposure {
				t.Fatalf("ApplyGoalState = %v, target exposure = %t", err, seenExposure)
			}
			if failure == "completion publication" {
				if err := os.Remove(filepath.Dir(op.startup.path)); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(filepath.Dir(op.startup.path)+"-saved", filepath.Dir(op.startup.path)); err != nil {
					t.Fatal(err)
				}
			}
			startup, loadErr := op.startup.Load()
			if loadErr != nil || startup == nil {
				t.Fatalf("journal = %+v, %v", startup, loadErr)
			}
			if failure == "SDK" || failure == "completion publication" {
				rolledBack := failure == "SDK"
				if f.active[container.UnitName("kube1")] != rolledBack || f.active[container.UnitName("kube2")] == rolledBack {
					t.Fatalf("unexpected SDK outcome for %s: %v", failure, f.active)
				}
				if _, err := os.Stat(f.snapshotPath()); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("successful SDK activation or rollback kept snapshot: %v", err)
				}
				if startup.Phase != "preparing" {
					t.Fatalf("ambiguous SDK outcome advanced journal: %+v", startup)
				}
				before := slices.Clone(f.events)
				if handled, err := op.ResumeRepave(t.Context(), discardLogger(), nil); !handled || err == nil {
					t.Fatalf("ambiguous SDK outcome was inferred safe: %t, %v", handled, err)
				}
				if !slices.Equal(f.events, before) {
					t.Fatal("fail-closed recovery mutated SDK state")
				}
				return
			}
			if failure == "NPD" {
				if startup.Phase != "starting" || store.state.ActiveMachine != "kube1" || f.active[container.UnitName("kube1")] || !f.active[container.UnitName("kube2")] {
					t.Fatalf("NPD failure did not leave stopped source and pending target: %+v, %v", startup, f.active)
				}
				if err := requireNoContainerStartupForAgentActivation(op.startup); err == nil {
					t.Fatal("pending NPD completion allowed source agent synchronization")
				}
				failure = "healthy"
			}
			observed := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: startup.Next.NodeConfig.Kubelet.Labels}}
			for range 2 {
				if _, err := op.ResumeRepave(t.Context(), discardLogger(), observed); err != nil {
					t.Fatal(err)
				}
			}
			if startup, err := op.startup.Load(); err != nil || startup != nil || store.state.ActiveMachine != "kube2" {
				t.Fatalf("completion did not converge: %+v, %v, state=%+v", startup, err, store.state)
			}
			if _, err := os.Stat(f.rootPath("kube1")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("completed repave kept source: %v", err)
			}
		})
	}
}
