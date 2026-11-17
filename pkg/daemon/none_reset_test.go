package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/internal/runtimeprovider"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/containerops"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostnode"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/noneops"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

func putNoneResetFile(t *testing.T, root, path, content string) string {
	t.Helper()
	path = filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newNoneResetTeardown(t *testing.T) *noneops.Teardown {
	t.Helper()
	root := t.TempDir()
	td := &noneops.Teardown{
		Log:        slog.New(slog.DiscardHandler),
		Layout:     none.Layout{Base: filepath.Join(root, "opt/unbounded/node")},
		SystemdDir: filepath.Join(root, "etc/systemd/system"),
		Root:       root,
		MountInfo:  putNoneResetFile(t, root, "mountinfo", ""),
		Proc:       filepath.Join(root, "proc"),
		CgroupRoot: filepath.Join(root, "cgroup"),
		Run: func(context.Context, *slog.Logger, string, ...string) (string, error) {
			return "", nil
		},
	}
	if err := os.MkdirAll(td.Proc, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := td.Layout.Ensure("v1"); err != nil {
		t.Fatal(err)
	}
	for _, unit := range []string{none.UnitContainerd, none.UnitKubelet, none.UnitLocalDNS, none.UnitKubeletLaunchCheck} {
		putNoneResetFile(t, td.SystemdDir, unit, "owned")
	}
	for _, path := range []string{
		"var/lib/cni/networks/unbounded/10.244.0.2",
		"var/lib/cni/results/pod-a",
		"etc/cni/net.d/10-unbounded.conflist",
		"var/lib/kubelet/pods/pod-a/state",
		"var/lib/containerd/state",
		none.ContainerdConfigPath,
		noneops.ModulesLoadPath,
	} {
		putNoneResetFile(t, root, path, "owned")
	}
	putNoneResetFile(t, td.CgroupRoot, "kubepods.slice/cgroup.kill", "0")
	return td
}

func assertNoneResetAbsent(t *testing.T, root string, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Lstat(filepath.Join(root, path)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s remains: %v", path, err)
		}
	}
}

func TestNoneResetSDKRemovesIPAMAfterDestroy(t *testing.T) {
	t.Parallel()
	td := newNoneResetTeardown(t)
	for _, path := range []string{goalstates.ContainerdConfigPath, "/opt/cni/bin/site-plugin"} {
		putNoneResetFile(t, td.Root, path, "site-owned")
	}
	cleanupCalls := 0
	td.Run = func(context.Context, *slog.Logger, string, ...string) (string, error) {
		if cleanupCalls == 0 {
			if _, err := os.Stat(filepath.Join(td.Root, hostnode.CNIStateDir)); err != nil {
				t.Fatalf("IPAM removed before SDK Destroy completed: %v", err)
			}
		}
		return "", nil
	}
	task := resetNoneRuntime(td.Log, td, func(root string) error {
		cleanupCalls++
		if root != td.Root || td.Present() {
			t.Fatalf("CNI cleanup root=%q, SDK still present=%v", root, td.Present())
		}
		assertNoneResetAbsent(t, root, "/var/lib/kubelet", "/var/lib/containerd", goalstates.CNIConfigDir)
		data, err := os.ReadFile(filepath.Join(td.CgroupRoot, "kubepods.slice/cgroup.kill"))
		if err != nil || string(data) != "1" {
			t.Fatalf("pod cgroups not ended before CNI cleanup: %q, %v", data, err)
		}
		return hostnode.ResetCNIState(root)
	})
	for pass := 1; pass <= 2; pass++ {
		if err := task.Do(t.Context()); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		assertNoneResetAbsent(t, td.Root, hostnode.CNIStateDir, none.ContainerdConfigPath, noneops.ModulesLoadPath)
		for _, path := range []string{goalstates.ContainerdConfigPath, "/opt/cni/bin/site-plugin"} {
			data, err := os.ReadFile(filepath.Join(td.Root, path))
			if err != nil || string(data) != "site-owned" {
				t.Fatalf("unowned %s changed: %q, %v", path, data, err)
			}
		}
	}
	if cleanupCalls != 2 {
		t.Fatalf("CNI cleanup calls=%d, want 2", cleanupCalls)
	}
}

func TestNoneResetSDKFailureKeepsIPAM(t *testing.T) {
	t.Parallel()
	for _, command := range []string{"systemctl stop " + none.UnitKubelet, "systemctl daemon-reload"} {
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			td := newNoneResetTeardown(t)
			failure := errors.New("SDK command failed")
			td.Run = func(_ context.Context, _ *slog.Logger, name string, args ...string) (string, error) {
				if name+" "+strings.Join(args, " ") == command {
					return "", failure
				}
				return "", nil
			}
			cleanupCalls := 0
			task := resetNoneRuntime(td.Log, td, func(root string) error {
				cleanupCalls++
				return hostnode.ResetCNIState(root)
			})
			if err := task.Do(t.Context()); !errors.Is(err, failure) {
				t.Fatalf("reset error=%v, want SDK failure", err)
			}
			if cleanupCalls != 0 {
				t.Fatalf("CNI cleanup ran %d times after SDK failure", cleanupCalls)
			}
			for _, path := range []string{"var/lib/cni/networks/unbounded/10.244.0.2", none.ContainerdConfigPath, noneops.ModulesLoadPath} {
				if _, err := os.Stat(filepath.Join(td.Root, path)); err != nil {
					t.Fatalf("%s removed after SDK failure: %v", path, err)
				}
			}
		})
	}
}

func TestNoneResetRetriesUsingAppliedStateAfterSDKFootprintsDisappear(t *testing.T) {
	t.Parallel()
	td := newNoneResetTeardown(t)
	store, err := newFileStateStore(filepath.Join(td.Root, "flex/daemon-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(t.Context(), &runtimeprovider.State{
		Isolation: isolation.ModeNone, NodeConfig: &agentconfig.AgentConfig{Isolation: string(isolation.ModeNone)},
	}); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("CNI cleanup failed")
	calls := 0
	runtime := resetNoneRuntime(td.Log, td, func(root string) error {
		calls++
		if td.Present() || root != td.Root {
			t.Fatalf("cleanup ran with SDK present or wrong root: %q", root)
		}
		if calls == 1 {
			return failure
		}
		return hostnode.ResetCNIState(root)
	})
	if err := runtime.Do(t.Context()); !errors.Is(err, failure) {
		t.Fatalf("initial reset=%v, want cleanup failure", err)
	}
	if td.Present() {
		t.Fatal("SDK footprints remain; test does not exercise applied-state routing")
	}
	if applied, err := store.Load(t.Context()); err != nil || applied == nil || applied.Isolation != isolation.ModeNone {
		t.Fatalf("failed reset lost applied state: %+v, %v", applied, err)
	}
	if _, err := os.Stat(filepath.Join(td.Root, hostnode.CNIStateDir)); err != nil {
		t.Fatalf("failed cleanup lost IPAM: %v", err)
	}
	for pass := 1; pass <= 2; pass++ {
		provider, err := discoverResetProvider(t.Context(), store, td.Log, nil)
		if err != nil || provider.Mode() != isolation.ModeNone {
			t.Fatalf("partial reset provider=%v, error=%v", provider, err)
		}
		if err := runtime.Do(t.Context()); err != nil {
			t.Fatalf("retry %d: %v", pass, err)
		}
		assertNoneResetAbsent(t, td.Root, hostnode.CNIStateDir, none.ContainerdConfigPath)
	}
	if calls != 3 {
		t.Fatalf("cleanup calls=%d, want 3", calls)
	}
}

func TestNoneResetCanceledAfterDestroyKeepsIPAM(t *testing.T) {
	t.Parallel()
	td := newNoneResetTeardown(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	td.Run = func(_ context.Context, _ *slog.Logger, name string, args ...string) (string, error) {
		if name+" "+strings.Join(args, " ") == "systemctl daemon-reload" {
			cancel()
		}
		return "", nil
	}
	err := resetNoneRuntime(td.Log, td, func(string) error {
		t.Fatal("CNI cleanup ignored cancellation")
		return nil
	}).Do(ctx)
	if !errors.Is(err, context.Canceled) || td.Present() {
		t.Fatalf("reset=%v, SDK present=%v", err, td.Present())
	}
	if _, err := os.Stat(filepath.Join(td.Root, hostnode.CNIStateDir)); err != nil {
		t.Fatalf("canceled reset removed IPAM: %v", err)
	}
}

func TestNoneResetRoutingPreservesContainerHostPrivatePaths(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	log := slog.New(slog.DiscardHandler)
	store, err := newFileStateStore(filepath.Join(root, "flex/daemon-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(t.Context(), containerTestState("kube1", "42")); err != nil {
		t.Fatal(err)
	}
	td := &containerops.Teardown{
		Log: log, Root: root, Layout: container.Layout{Base: filepath.Join(root, "opt/unbounded/containers")},
		SystemdDir:  filepath.Join(root, "etc/systemd/system"),
		RuntimePath: filepath.Join(root, "opt/unbounded/runtime/runc"),
		MountInfo:   putNoneResetFile(t, root, "mountinfo", ""),
		Run: func(context.Context, *slog.Logger, string, ...string) (string, error) {
			t.Fatal("container reset without slots must not run host commands")
			return "", nil
		},
	}
	paths := []string{
		"etc/cni/net.d/site.conflist", "var/lib/cni/networks/site/10.0.0.2",
		"var/lib/containerd/site", goalstates.ContainerdConfigPath,
	}
	for _, path := range paths {
		putNoneResetFile(t, root, path, "site-owned")
	}
	if td.Present() {
		t.Fatal("container footprints remain; test must exercise applied-state routing")
	}
	containerTask := phases.Serial(log, td.Stop(), td.Destroy(), td.RemoveConfig())
	for pass := 1; pass <= 2; pass++ {
		provider, err := discoverResetProvider(t.Context(), store, log, nil)
		if err != nil || provider.Mode() != isolation.ModeContainer {
			t.Fatalf("partial reset provider=%v, error=%v", provider, err)
		}
		if err := containerTask.Do(t.Context()); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		for _, path := range paths {
			data, err := os.ReadFile(filepath.Join(root, path))
			if err != nil || string(data) != "site-owned" {
				t.Fatalf("unowned container host path %s changed: %q, %v", path, data, err)
			}
		}
	}
}
