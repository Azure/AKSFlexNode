package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/containerops"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
)

type preparedTestNode struct {
	params  containerops.Params
	backend container.Backend
}

func (n *preparedTestNode) GoalState() (*goalstates.MachineGoalState, error) {
	return n.params.GoalState, nil
}

func (n *preparedTestNode) Backend() *container.Backend { return &n.backend }

func TestPreparedContainerPathsCoverPrivateAndSharedState(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	root := container.Root{SlotName: "kube2", Dir: filepath.Join(base, "containers/kube2"), HostRoot: filepath.Join(base, "host")}
	if err := os.MkdirAll(root.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	node := &preparedTestNode{
		params: containerops.Params{GoalState: &goalstates.MachineGoalState{RootFS: &goalstates.RootFS{RootOverride: root}}},
		backend: container.Backend{Layout: container.Layout{Base: filepath.Join(base, "containers")},
			SystemdDir: filepath.Join(base, "systemd"), RuntimePath: filepath.Join(base, "runtime/runc"), HelperPath: filepath.Join(base, "agent/bin/aks-flex-node")},
	}
	paths, err := preparedContainerPaths(node)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{root.Dir, filepath.Join(base, "containers/bundles/kube2"), node.backend.SystemdDir,
		filepath.Dir(node.backend.RuntimePath), filepath.Dir(node.backend.HelperPath), root.Path("/var/lib/kubelet"), root.Path("/opt/cni"), root.Path("/etc/containerd/certs.d")} {
		if !slices.Contains(paths, required) {
			t.Errorf("durability barrier omits %s: %v", required, paths)
		}
	}
}

func TestPreparedFilesystemBarrier(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		failSync bool
		cancel   bool
	}{
		{name: "deduplicates filesystem and persists removed paths"},
		{name: "sync failure", failSync: true},
		{name: "cancellation", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := t.TempDir()
			dir := filepath.Join(base, "prepared")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			syncErr := errors.New("filesystem persistence failed")
			calls := 0
			err := syncPreparedFilesystems(ctx, []string{dir, filepath.Join(dir, "removed-identity/client.pem"), base}, func(f *os.File) error {
				calls++
				if _, err := f.Stat(); err != nil {
					t.Fatalf("barrier did not keep its filesystem descriptor open: %v", err)
				}
				if tc.cancel {
					cancel()
				}
				if tc.failSync {
					return syncErr
				}
				return nil
			})
			if calls != 1 {
				t.Fatalf("one filesystem was flushed %d times", calls)
			}
			switch {
			case tc.failSync:
				if !errors.Is(err, syncErr) {
					t.Fatalf("sync failure was lost: %v", err)
				}
			case tc.cancel:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation was lost: %v", err)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestPreparedFilesystemBarrierRejectsInvalidPaths(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"relative", "not a directory"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := "relative/path"
			if name == "not a directory" {
				file := filepath.Join(t.TempDir(), "file")
				if err := os.WriteFile(file, []byte("data"), 0o600); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(file, "child")
			}
			if err := syncPreparedFilesystems(t.Context(), []string{path}, func(*os.File) error {
				t.Fatal("invalid path reached filesystem flush")
				return nil
			}); err == nil {
				t.Fatal("invalid path was silently ignored")
			}
		})
	}
}
