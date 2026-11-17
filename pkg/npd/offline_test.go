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
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

func TestOfflineNPDUsesChosenRootAndUnit(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"nspawn", "container", "none"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				name, failAction, cancelAfter string
				absent, statFailure           bool
				failure                       error
			}{
				{name: "owned unit"},
				{name: "fresh root", absent: true},
				{name: "stop failure", failAction: "stop", failure: errors.New("stop failed")},
				{name: "disable failure", failAction: "disable", failure: errors.New("disable failed")},
				{name: "executor canceled", failAction: "stop", failure: context.Canceled},
				{name: "executor deadline", failAction: "disable", failure: context.DeadlineExceeded},
				{name: "canceled before cleanup", cancelAfter: "before"},
				{name: "canceled after stop", cancelAfter: "stop"},
				{name: "canceled after disable", cancelAfter: "disable"},
				{name: "ownership stat failure", statFailure: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					var root isolation.NodeRoot = isolation.DirRoot{Dir: t.TempDir()}
					switch mode {
					case "container":
						root = container.Root{SlotName: "worker", Dir: t.TempDir(), HostRoot: t.TempDir()}
					case "none":
						root = none.Root{HostRoot: t.TempDir(), Layout: none.Layout{Base: t.TempDir()}, Version: "offline"}
					}
					const unit = "chosen-worker-npd.service"
					unitPath := root.Path("/etc/systemd/system/" + unit)
					if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
						t.Fatal(err)
					}
					owned := []byte("owned unit retained for retry")
					if tc.statFailure {
						if err := os.Symlink(unitPath, unitPath); err != nil {
							t.Fatal(err)
						}
					} else if !tc.absent {
						if err := os.WriteFile(unitPath, owned, 0o644); err != nil {
							t.Fatal(err)
						}
					}
					decoyRoot := isolation.DirRoot{Dir: t.TempDir()}
					decoyUnit := decoyRoot.Path("/etc/systemd/system/" + unit)
					if err := os.MkdirAll(filepath.Dir(decoyUnit), 0o755); err != nil {
						t.Fatal(err)
					}
					untouched := []string{
						decoyUnit,
						root.Path("/etc/systemd/system/" + systemdUnitNPD),
						root.Path("/etc/systemd/system/" + HostUnitNPD),
					}
					for _, path := range untouched {
						if err := os.WriteFile(path, []byte("unrelated service"), 0o644); err != nil {
							t.Fatal(err)
						}
					}
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					if tc.cancelAfter == "before" {
						cancel()
					}
					log := slog.New(slog.DiscardHandler)
					var calls []string
					failAction, cancelAfter := tc.failAction, tc.cancelAfter
					ns := &goalstates.NodeStart{
						RootOverride: decoyRoot,
						ExecOverride: func(gotCtx context.Context, gotLog *slog.Logger, args ...string) (string, error) {
							if gotCtx != ctx || gotLog != log {
								t.Fatal("offline NPD did not propagate context and logger")
							}
							if len(args) != 3 || args[0] != "systemctl" || args[2] != unit {
								t.Fatalf("unexpected cleanup command: %v", args)
							}
							calls = append(calls, strings.Join(args, " "))
							if args[1] == failAction {
								return "", tc.failure
							}
							if args[1] == cancelAfter {
								cancel()
							}
							return "", nil
						},
					}
					cfg := &config.Config{Bootstrap: config.BootstrapConfig{
						OfflineArtifacts: config.OfflineArtifactsConfig{Source: "https://example.invalid/artifacts"},
					}}
					task := Start(log, cfg, root, ns, StartOptions{UnitName: unit})
					if task.Name() != "start-npd" {
						t.Fatalf("task name = %q", task.Name())
					}
					wantErr := tc.failure
					if tc.cancelAfter != "" {
						wantErr = context.Canceled
					}
					if tc.statFailure {
						_, err := os.Stat(unitPath)
						var pathErr *os.PathError
						if !errors.As(err, &pathErr) {
							t.Fatalf("expected stat failure, got %v", err)
						}
						wantErr = pathErr.Err
					}
					if err := task.Do(ctx); !errors.Is(err, wantErr) {
						t.Fatalf("cleanup error = %v, want %v", err, wantErr)
					}
					var want []string
					if !tc.absent && !tc.statFailure && tc.cancelAfter != "before" {
						want = append(want, "systemctl stop "+unit)
						if tc.failAction != "stop" && tc.cancelAfter != "stop" {
							want = append(want, "systemctl disable "+unit)
						}
					}
					if !slices.Equal(calls, want) {
						t.Fatalf("cleanup calls = %v, want %v", calls, want)
					}
					if tc.statFailure {
						if err := os.Remove(unitPath); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(unitPath, owned, 0o644); err != nil {
							t.Fatal(err)
						}
					}
					ctx, failAction, cancelAfter = t.Context(), "", ""
					for range 2 {
						calls = nil
						if err := task.Do(ctx); err != nil {
							t.Fatalf("cleanup retry: %v", err)
						}
						want = nil
						if !tc.absent {
							want = []string{"systemctl stop " + unit, "systemctl disable " + unit}
						}
						if !slices.Equal(calls, want) {
							t.Fatalf("retry calls = %v, want %v", calls, want)
						}
					}
					if tc.absent {
						if _, err := os.Stat(unitPath); !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("fresh offline root gained a unit: %v", err)
						}
					} else if got, err := os.ReadFile(unitPath); err != nil || !bytes.Equal(got, owned) {
						t.Fatalf("ownership record changed: %q, %v", got, err)
					}
					for _, path := range untouched {
						if got, err := os.ReadFile(path); err != nil || string(got) != "unrelated service" {
							t.Fatalf("unrelated service changed: %s: %v", path, err)
						}
					}
				})
			}
		})
	}
}
