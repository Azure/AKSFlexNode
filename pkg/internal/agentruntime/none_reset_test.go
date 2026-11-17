package agentruntime

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostnode"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/noneops"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

func testNoneTeardown(t *testing.T, root, mountInfo string) *noneTeardown {
	t.Helper()

	log := slog.New(slog.DiscardHandler)
	return &noneTeardown{
		log: log,
		sdk: &noneops.Teardown{
			Log:        log,
			Root:       root,
			Layout:     none.Layout{Base: filepath.Join(root, "node")},
			SystemdDir: filepath.Join(root, "units"),
			MountInfo:  mountInfo,
			Proc:       filepath.Join(root, "proc"),
			Run: func(context.Context, *slog.Logger, string, ...string) (string, error) {
				return "", nil
			},
		},
	}
}

func writeResetFile(t *testing.T, path string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestNoneLateFailureRetainsOwnershipAndRetries(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	mountInfo := filepath.Join(root, "mountinfo")
	first := testNoneTeardown(t, root, mountInfo)
	if err := os.WriteFile(mountInfo, []byte("1 0 0:1 / /var/lib/kubelet/pods/test rw - tmpfs tmpfs rw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first.sdk.Run = func(_ context.Context, _ *slog.Logger, command string, _ ...string) (string, error) {
		if command == "umount" {
			return "", errors.New("unmount failed")
		}
		return "", nil
	}
	if err := os.MkdirAll(first.sdk.Proc, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := first.sdk.Layout.Ensure("version"); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(first.sdk.SystemdDir, none.UnitKubelet)
	writeResetFile(t, unit)
	data := filepath.Join(root, "var/lib/kubelet/data")
	writeResetFile(t, data)
	journal := filepath.Join(root, goalstates.AgentConfigDir, "none-upgrade-journal.json")
	writeResetFile(t, journal)

	if !first.Present() {
		t.Fatal("SDK did not discover installed worker")
	}
	if err := first.Stop().Do(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := first.Destroy().Do(t.Context()); err == nil {
		t.Fatal("destroy succeeded despite failed unmount")
	}
	if _, err := os.Lstat(unit); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SDK did not remove unit: %v", err)
	}
	if _, err := os.Lstat(first.sdk.Layout.VersionDir("version")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SDK did not remove version layout: %v", err)
	}
	for _, path := range []string{data, journal} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("retry artifact %s lost: %v", path, err)
		}
	}

	fresh := testNoneTeardown(t, root, mountInfo)
	if !fresh.Present() {
		t.Fatal("fresh SDK teardown could not rediscover ownership")
	}
	if err := os.WriteFile(mountInfo, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := phases.Serial(fresh.log, fresh.Stop(), fresh.Destroy()).Do(t.Context()); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if fresh.Present() {
		t.Fatal("ownership trace remains after successful retry")
	}
	for _, path := range []string{data, journal} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("retry artifact %s remains: %v", path, err)
		}
	}
}

func TestOrphanArtifactsDoNotSelectNativeTeardown(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	native := testNoneTeardown(t, root, filepath.Join(root, "mountinfo"))
	dir := filepath.Join(root, goalstates.AgentConfigDir)
	journal := filepath.Join(dir, "none-upgrade-journal.json")
	snapshot := filepath.Join(dir, "none-upgrade-config", "credentials")
	unrelated := filepath.Join(dir, "unrelated")
	kubelet := filepath.Join(root, "var/lib/kubelet/private")
	containerd := filepath.Join(root, "var/lib/containerd/private")
	npd := filepath.Join(root, "etc/systemd/system/node-problem-detector.service")
	for _, path := range []string{journal, snapshot, unrelated, kubelet, containerd, npd} {
		writeResetFile(t, path)
	}

	var events []string
	machine := &testResetWorker{name: "nspawn", events: &events}
	shared := hostnode.Func("shared", func(context.Context) error { return nil })
	for range 2 {
		plan := discoverReset(native.log, machine, native, shared, cleanupNoneUpgradeArtifacts(root), true)
		if plan.none != nil {
			t.Fatal("recovery artifacts incorrectly imply SDK ownership")
		}
		if !plan.HasHostNativeState() {
			t.Fatal("orphan artifacts would skip independently owned host-component cleanup")
		}
		if err := phases.Serial(native.log, plan.Stop(), plan.Finish()).Do(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{journal, snapshot} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("orphan artifact %s remains: %v", path, err)
		}
	}
	for _, path := range []string{unrelated, kubelet, containerd, npd} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("unowned file %s removed: %v", path, err)
		}
	}
}

func TestNoneStopFailureKeepsRecoveryArtifacts(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	task := testNoneTeardown(t, root, filepath.Join(root, "mountinfo"))
	writeResetFile(t, filepath.Join(task.sdk.SystemdDir, none.UnitKubelet))
	journal := filepath.Join(root, goalstates.AgentConfigDir, "none-upgrade-journal.json")
	writeResetFile(t, journal)
	task.sdk.Run = func(context.Context, *slog.Logger, string, ...string) (string, error) {
		return "", errors.New("stop failed")
	}
	if err := task.Stop().Do(t.Context()); err == nil {
		t.Fatal("stop succeeded")
	}
	if _, err := os.Stat(journal); err != nil {
		t.Fatalf("recovery journal removed: %v", err)
	}
}
