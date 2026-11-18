package agentruntime

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/containerops"
	"github.com/Azure/unbounded/pkg/agent/hostnode/hostnodetest"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
)

type cleanupRetryFixture struct {
	layout     container.Layout
	root       string
	systemd    string
	mountInfo  string
	busy       error
	unpackErr  error
	cleanupErr error
	unmounts   int
	unpacks    int
	calls      []string
	onUnmount  func(context.Context)
	onUnpack   func(string) error
}

func newCleanupRetryFixture(t *testing.T, slot string) *cleanupRetryFixture {
	t.Helper()

	dir := t.TempDir()
	f := &cleanupRetryFixture{
		layout:    container.Layout{Base: filepath.Join(dir, "layout", "containers")},
		root:      filepath.Join(dir, "host"),
		systemd:   filepath.Join(dir, "systemd"),
		mountInfo: filepath.Join(dir, "mountinfo"),
		busy:      errors.New("bpffs busy"),
		unpackErr: errors.New("unpack reached"),
	}
	bpffs := filepath.Join(f.root, container.BPFFSPath(slot))
	for _, path := range []string{f.layout.RootfsDir(slot), f.systemd, bpffs} {
		if err := os.MkdirAll(path, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.systemd, container.UnitName(slot)), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	mount := "40 30 0:40 / " + bpffs + " rw - bpf bpf rw\n"
	if err := os.WriteFile(f.mountInfo, []byte(mount), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *cleanupRetryFixture) run(ctx context.Context, _ *slog.Logger, name string, args ...string) (string, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if name == "umount" {
		f.unmounts++
		if f.onUnmount != nil {
			f.onUnmount(ctx)
		}
		if f.busy != nil {
			return "", f.busy
		}
		if err := os.WriteFile(f.mountInfo, nil, 0o600); err != nil {
			return "", err
		}
	}
	if strings.HasSuffix(name, "runc") && len(args) > 0 && args[0] == "state" {
		return "", errors.New("container absent")
	}
	return "", ctx.Err()
}

func (f *cleanupRetryFixture) runtime() containerRuntime {
	return containerRuntime{
		stageArchives: stageTestArchives,
		newNode: func(params containerops.Params) *containerops.Node {
			params.Layout = f.layout
			params.SystemdDir = f.systemd
			params.RuntimePath = filepath.Join(f.root, "runc")
			params.MountInfoPath = f.mountInfo
			params.HostRoot = f.root
			params.Run = f.run
			params.InstallRuntime = func(context.Context) error { return nil }
			params.InstallHelper = func(context.Context) error { return nil }
			params.ProvisionRootfs = func(context.Context) error { return nil }
			params.CleanupNetwork = func(context.Context) error { return f.cleanupErr }
			params.Unpack = func(_ context.Context, path string) error {
				f.unpacks++
				if f.onUnpack != nil {
					return f.onUnpack(path)
				}
				return f.unpackErr
			}
			return containerops.New(params)
		},
	}
}

func cleanupInput(target, previous string) Inputs {
	old := Node{Slot: previous, GoalState: hostnodetest.GoalState(previous, false)}
	return Inputs{
		Config: &config.Config{},
		Node: Node{
			Slot:      target,
			GoalState: hostnodetest.GoalState(target, false),
		},
		Previous: &old,
	}
}

func assertCleanupTrace(t *testing.T, f *cleanupRetryFixture, slot string) {
	t.Helper()

	backend := &container.Backend{Layout: f.layout, SystemdDir: f.systemd}
	if !backend.SlotExists(slot) {
		t.Fatalf("SDK does not detect failed slot %s", slot)
	}
	if _, err := os.Stat(filepath.Join(f.root, container.BPFFSPath(slot))); err != nil {
		t.Fatalf("bpffs path not retained: %v", err)
	}
}

func TestContainerRetireAndStaleSlotCleanupRetry(t *testing.T) {
	t.Parallel()

	for _, slot := range []string{"kube1", "kube2"} {
		t.Run(slot, func(t *testing.T) {
			t.Parallel()

			f := newCleanupRetryFixture(t, slot)
			log := slog.New(slog.DiscardHandler)
			other := container.OtherSlot(slot)
			retire := cleanupInput(other, slot)
			if err := f.runtime().Retire(log, retire).Do(t.Context()); err != nil {
				t.Fatalf("retire should remain best effort: %v", err)
			}
			assertCleanupTrace(t, f, slot)

			for attempt := 0; attempt < 2; attempt++ {
				err := f.runtime().Start(log, cleanupInput(slot, other)).Do(t.Context())
				if !errors.Is(err, f.busy) || !strings.Contains(err.Error(), "remove stale slot") {
					t.Fatalf("stale cleanup error = %v", err)
				}
				assertCleanupTrace(t, f, slot)
			}
			if f.unmounts != 3 || f.unpacks != 0 {
				t.Fatalf("unmounts=%d unpacks=%d", f.unmounts, f.unpacks)
			}
			for _, call := range f.calls {
				if strings.Contains(call, "systemctl stop "+container.UnitName(other)) {
					t.Fatalf("stopped active slot during stale cleanup: %s", call)
				}
			}

			f.busy = nil
			err := f.runtime().Start(log, cleanupInput(slot, other)).Do(t.Context())
			if !errors.Is(err, f.unpackErr) || f.unmounts != 4 || f.unpacks != 1 {
				t.Fatalf("retry error=%v unmounts=%d unpacks=%d", err, f.unmounts, f.unpacks)
			}
			if _, err := os.Stat(filepath.Join(f.root, container.BPFFSPath(slot))); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("bpffs remains after successful cleanup: %v", err)
			}
		})
	}
}

func TestContainerStaleCleanupPreservesCancellation(t *testing.T) {
	t.Parallel()

	f := newCleanupRetryFixture(t, "kube1")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := f.runtime().Start(slog.New(slog.DiscardHandler), cleanupInput("kube1", "kube2")).Do(ctx)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, f.busy) {
		t.Fatalf("cleanup error = %v", err)
	}
	assertCleanupTrace(t, f, "kube1")
}

func TestContainerUpgradeSameSlotDoesNotDestroy(t *testing.T) {
	t.Parallel()

	f := newCleanupRetryFixture(t, "kube1")
	err := f.runtime().Start(slog.New(slog.DiscardHandler), cleanupInput("kube1", "kube1")).Do(t.Context())
	if err == nil || !strings.Contains(err.Error(), "different slot") || f.unmounts != 0 {
		t.Fatalf("upgrade error=%v unmounts=%d", err, f.unmounts)
	}
	assertCleanupTrace(t, f, "kube1")
}

func TestContainerStaleCleanupRetainsTraceAfterUnmount(t *testing.T) {
	t.Parallel()

	f := newCleanupRetryFixture(t, "kube1")
	log := slog.New(slog.DiscardHandler)
	if err := f.runtime().Retire(log, cleanupInput("kube2", "kube1")).Do(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.busy = nil
	bpffs := filepath.Join(f.root, container.BPFFSPath("kube1"))
	blocker := filepath.Join(bpffs, "blocked")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		err := f.runtime().Start(log, cleanupInput("kube1", "kube2")).Do(t.Context())
		if err == nil || !strings.Contains(err.Error(), "remove bpffs mount point") {
			t.Fatalf("stale cleanup error = %v", err)
		}
		assertCleanupTrace(t, f, "kube1")
	}
	if f.unmounts != 2 || f.unpacks != 0 {
		t.Fatalf("unmounts=%d unpacks=%d", f.unmounts, f.unpacks)
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	err := f.runtime().Start(log, cleanupInput("kube1", "kube2")).Do(t.Context())
	if !errors.Is(err, f.unpackErr) || f.unpacks != 1 {
		t.Fatalf("retry error=%v unpacks=%d", err, f.unpacks)
	}
}

func TestContainerUpgradeAbandonRetainsTraceWithCanceledContext(t *testing.T) {
	t.Parallel()

	f := newCleanupRetryFixture(t, "kube2")
	if err := os.RemoveAll(f.layout.RootfsDir("kube2")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.systemd, container.UnitName("kube2"))); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.onUnpack = func(path string) error {
		if err := os.MkdirAll(path, 0o750); err != nil {
			return err
		}
		cancel()
		return f.unpackErr
	}
	f.onUnmount = func(ctx context.Context) {
		if err := ctx.Err(); err != nil {
			t.Fatalf("SDK abandon cleanup lost its uncanceled context: %v", err)
		}
	}
	err := f.runtime().Start(slog.New(slog.DiscardHandler), cleanupInput("kube2", "kube1")).Do(ctx)
	if !errors.Is(err, f.unpackErr) || !errors.Is(err, f.busy) || f.unmounts != 1 {
		t.Fatalf("abandon error=%v unmounts=%d", err, f.unmounts)
	}
	assertCleanupTrace(t, f, "kube2")
}

func TestContainerUpgradeRollbackRetainsTrace(t *testing.T) {
	t.Parallel()

	f := newCleanupRetryFixture(t, "kube2")
	if err := os.RemoveAll(f.layout.RootfsDir("kube2")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.systemd, container.UnitName("kube2"))); err != nil {
		t.Fatal(err)
	}
	f.onUnpack = func(path string) error { return os.MkdirAll(path, 0o750) }
	f.cleanupErr = errors.New("network cleanup failed")
	input := cleanupInput("kube2", "kube1")
	input.Config.Bootstrap.OfflineArtifacts.Source = "offline"
	err := f.runtime().Start(slog.New(slog.DiscardHandler), input).Do(t.Context())
	if !errors.Is(err, f.cleanupErr) || !errors.Is(err, f.busy) || !strings.Contains(err.Error(), "previous slot kube1 is left stopped") {
		t.Fatalf("rollback error = %v", err)
	}
	assertCleanupTrace(t, f, "kube2")
}

func TestContainerResetRetriesFailedBPFFSCleanup(t *testing.T) {
	t.Parallel()

	f := newCleanupRetryFixture(t, "kube1")
	for attempt := 0; attempt < 3; attempt++ {
		teardown := &containerops.Teardown{
			Log:         slog.New(slog.DiscardHandler),
			Layout:      f.layout,
			SystemdDir:  f.systemd,
			Root:        f.root,
			RuntimePath: filepath.Join(f.root, "runc"),
			MountInfo:   f.mountInfo,
			Run:         f.run,
		}
		if !teardown.Present() {
			t.Fatal("fresh reset did not discover OCI")
		}
		err := containerTeardown(teardown).Do(t.Context())
		if !errors.Is(err, f.busy) {
			t.Fatalf("reset error = %v", err)
		}
		assertCleanupTrace(t, f, "kube1")
	}
	f.busy = nil
	teardown := &containerops.Teardown{
		Log:         slog.New(slog.DiscardHandler),
		Layout:      f.layout,
		SystemdDir:  f.systemd,
		Root:        f.root,
		RuntimePath: filepath.Join(f.root, "runc"),
		MountInfo:   f.mountInfo,
		Run:         f.run,
	}
	if err := containerTeardown(teardown).Do(t.Context()); err != nil {
		t.Fatalf("reset retry: %v", err)
	}
	if teardown.Present() || f.unmounts != 4 {
		t.Fatalf("reset retained ownership=%t unmounts=%d", teardown.Present(), f.unmounts)
	}
}

func TestContainerCleanupReportsTraceRetentionFailure(t *testing.T) {
	t.Parallel()

	for _, operation := range []string{"retire", "upgrade", "reset"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()

			f := newCleanupRetryFixture(t, "kube1")
			f.onUnmount = func(context.Context) {
				parent := filepath.Dir(f.layout.Base)
				if err := os.RemoveAll(parent); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(parent, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			log := slog.New(slog.DiscardHandler)
			var err error
			switch operation {
			case "retire":
				err = f.runtime().Retire(log, cleanupInput("kube2", "kube1")).Do(t.Context())
			case "upgrade":
				err = f.runtime().Start(log, cleanupInput("kube1", "kube2")).Do(t.Context())
			case "reset":
				teardown := &containerops.Teardown{
					Log:         log,
					Layout:      f.layout,
					SystemdDir:  f.systemd,
					Root:        f.root,
					RuntimePath: filepath.Join(f.root, "runc"),
					MountInfo:   f.mountInfo,
					Run:         f.run,
				}
				err = containerTeardown(teardown).Do(t.Context())
			}
			if !errors.Is(err, f.busy) || !strings.Contains(err.Error(), "retain OCI slot kube1 ownership") {
				t.Fatalf("cleanup error = %v", err)
			}
		})
	}
}

func TestRetainContainerSlotLeavesCleanFailuresAlone(t *testing.T) {
	t.Parallel()

	f := newCleanupRetryFixture(t, "kube2")
	if err := os.RemoveAll(filepath.Join(f.root, container.BPFFSPath("kube2"))); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(f.layout.RootfsDir("kube2")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.systemd, container.UnitName("kube2"))); err != nil {
		t.Fatal(err)
	}
	backend := &container.Backend{Layout: f.layout, SystemdDir: f.systemd, SourceRoot: f.root}
	if err := retainContainerBPFFSSlot(backend, "kube2"); err != nil {
		t.Fatal(err)
	}
	if backend.SlotExists("kube2") {
		t.Fatal("created a slot trace without a bpffs path")
	}
}
