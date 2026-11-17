package runtimeprovider

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/noneops"
)

func TestResetNoneRuntimeCleanupOrder(t *testing.T) {
	t.Parallel()
	stopErr, cleanupErr := errors.New("stop failed"), errors.New("CNI cleanup failed")
	for _, tc := range []struct {
		name       string
		stopErr    error
		cleanupErr error
		cancel     bool
		wantErr    error
	}{
		{name: "success"},
		{name: "stop failure", stopErr: stopErr, wantErr: stopErr},
		{name: "CNI cleanup failure", cleanupErr: cleanupErr, wantErr: cleanupErr},
		{name: "canceled", cancel: true, wantErr: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			unitDir := filepath.Join(root, "etc/systemd/system")
			module := filepath.Join(root, noneops.ModulesLoadPath)
			for _, dir := range []string{unitDir, filepath.Dir(module)} {
				if err := os.MkdirAll(dir, 0o750); err != nil {
					t.Fatal(err)
				}
			}
			unit := filepath.Join(unitDir, none.UnitKubelet)
			for _, path := range []string{unit, module} {
				if err := os.WriteFile(path, []byte("owned"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			mountInfo := filepath.Join(root, "mountinfo")
			if err := os.WriteFile(mountInfo, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			teardown := &noneops.Teardown{
				Log: slog.New(slog.DiscardHandler), Root: root,
				Layout: none.Layout{Base: filepath.Join(root, "node")}, SystemdDir: unitDir,
				MountInfo: mountInfo, Proc: t.TempDir(), CgroupRoot: t.TempDir(),
				Run: func(ctx context.Context, log *slog.Logger, name string, args ...string) (string, error) {
					if name == "systemctl" && len(args) > 0 && args[0] == "stop" {
						return "", tc.stopErr
					}
					return "", nil
				},
			}
			calls := 0
			task := ResetNoneRuntime(teardown.Log, teardown, func(gotRoot string) error {
				calls++
				if gotRoot != root {
					t.Errorf("CNI root = %q, want %q", gotRoot, root)
				}
				if _, err := os.Stat(unit); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("CNI cleanup ran before runtime destruction: %v", err)
				}
				if calls == 1 {
					if _, err := os.Stat(module); err != nil {
						t.Errorf("runtime configuration removed before CNI cleanup: %v", err)
					}
				}
				return tc.cleanupErr
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			if err := task.Do(ctx); !errors.Is(err, tc.wantErr) {
				t.Fatalf("reset = %v, want %v", err, tc.wantErr)
			}
			if tc.stopErr != nil || tc.cancel {
				if calls != 0 {
					t.Fatalf("CNI cleanup ran after an earlier failure: %d calls", calls)
				}
			} else if calls != 1 {
				t.Fatalf("CNI cleanup called %d times, want 1", calls)
			}
			_, err := os.Stat(module)
			if tc.wantErr != nil {
				if err != nil {
					t.Fatalf("failed reset lost its configuration: %v", err)
				}
				return
			}
			if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("successful reset retained configuration: %v", err)
			}
			if err := task.Do(t.Context()); err != nil || calls != 2 {
				t.Fatalf("repeated reset = %v, CNI cleanup calls = %d", err, calls)
			}
		})
	}
}
