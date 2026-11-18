package agentruntime

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/hostnode"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/noneops"
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

func TestNoneDestroyPropagatesUnmountFailure(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	mountInfo := filepath.Join(root, "mountinfo")
	task := testNoneTeardown(t, root, mountInfo)
	if err := os.WriteFile(mountInfo, []byte("1 0 0:1 / /var/lib/kubelet/pods/test rw - tmpfs tmpfs rw\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	unmountErr := errors.New("unmount failed")
	task.sdk.Run = func(_ context.Context, _ *slog.Logger, command string, _ ...string) (string, error) {
		if command == "umount" {
			return "", unmountErr
		}
		return "", nil
	}
	if err := os.MkdirAll(task.sdk.Proc, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := task.sdk.Layout.Ensure("version"); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(task.sdk.SystemdDir, none.UnitKubelet)
	writeResetFile(t, unit)
	data := filepath.Join(root, "var/lib/kubelet/data")
	writeResetFile(t, data)

	if !task.Present() {
		t.Fatal("SDK did not discover installed worker")
	}
	if err := task.Stop().Do(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := task.Destroy().Do(t.Context()); !errors.Is(err, unmountErr) {
		t.Fatalf("destroy error = %v, want unmount failure", err)
	}
	if _, err := os.Lstat(unit); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SDK did not remove unit: %v", err)
	}
	if _, err := os.Lstat(task.sdk.Layout.VersionDir("version")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SDK did not remove version layout: %v", err)
	}
	if _, err := os.Stat(data); err != nil {
		t.Fatalf("mounted data lost: %v", err)
	}
}

func TestNoneDestroyCleansCNIState(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	mountInfo := filepath.Join(root, "mountinfo")
	task := testNoneTeardown(t, root, mountInfo)
	if err := os.WriteFile(mountInfo, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(task.sdk.Proc, 0o700); err != nil {
		t.Fatal(err)
	}

	cniState := filepath.Join(root, hostnode.CNIStateDir, "networks", "bridge", "reservation")
	writeResetFile(t, cniState)

	if err := task.Destroy().Do(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cniState); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("CNI state remains: %v", err)
	}
}

func TestNoneStopFailure(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	task := testNoneTeardown(t, root, filepath.Join(root, "mountinfo"))
	writeResetFile(t, filepath.Join(task.sdk.SystemdDir, none.UnitKubelet))

	stopErr := errors.New("stop failed")
	task.sdk.Run = func(context.Context, *slog.Logger, string, ...string) (string, error) {
		return "", stopErr
	}

	if err := task.Stop().Do(t.Context()); !errors.Is(err, stopErr) {
		t.Fatalf("stop error = %v, want stop failure", err)
	}
}
