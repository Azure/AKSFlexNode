package agentruntime

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/hostnode"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

func stagedOCIReset(t *testing.T, f *containerTeardownFixture) ResetPlan {
	t.Helper()

	var events []string
	nspawn := &testResetWorker{name: "nspawn", events: &events}
	native := &testResetWorker{name: "none", events: &events}
	shared := hostnode.Func("shared", func(context.Context) error { return nil })
	artifacts := hostnode.Func("artifacts", func(context.Context) error { return nil })
	return discoverReset(f.teardown.Log, nspawn, native, &ociTeardown{sdk: f.teardown}, shared, artifacts, false)
}

func TestOCIStagedResetToleratesMissingSupervisor(t *testing.T) {
	t.Parallel()

	for _, retry := range []bool{false, true} {
		name := "interrupted prepare without unit"
		if retry {
			name = "retry after destroy removed unit"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newContainerTeardownFixture(t)
			f.teardown.Run = func(ctx context.Context, log *slog.Logger, command string, args ...string) (string, error) {
				if command == "systemctl" && len(args) == 4 && args[0] == "show" && args[1] == "--property=LoadState" {
					return "not-found\n", nil
				}
				return f.run(ctx, log, command, args...)
			}
			if retry {
				if err := os.WriteFile(f.unit, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				mount := "100 1 0:50 / " + f.teardown.Layout.RootfsDir("kube1") + "/volume rw - xfs /dev/sda rw\n"
				if err := os.WriteFile(f.teardown.MountInfo, []byte(mount), 0o600); err != nil {
					t.Fatal(err)
				}
				first := stagedOCIReset(t, f)
				if err := phases.Serial(f.teardown.Log, first.Stop(), first.Finish()).Do(t.Context()); err == nil || !strings.Contains(err.Error(), "still mounted") {
					t.Fatalf("initial reset error = %v, want mounted-rootfs failure", err)
				}
				if err := os.WriteFile(f.teardown.MountInfo, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := os.Stat(f.unit); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("supervisor unexpectedly present: %v", err)
			}

			fresh := stagedOCIReset(t, f)
			if err := fresh.Stop().Do(t.Context()); err != nil {
				t.Fatalf("staged stop without supervisor: %v", err)
			}
			for _, path := range []string{f.snapshot, f.sharedConfig, f.teardown.Layout.RootfsDir("kube1")} {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("Stop removed %s before Finish: %v", path, err)
				}
			}
			if err := fresh.Finish().Do(t.Context()); err != nil {
				t.Fatalf("staged cleanup: %v", err)
			}
			for _, path := range []string{f.snapshot, f.sharedConfig, f.teardown.Layout.RootfsDir("kube1")} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("Finish retained %s: %v", path, err)
				}
			}
		})
	}
}

func TestOCIStagedStopPreservesRealFailures(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		unit       bool
		alive      bool
		inspectErr bool
	}{
		{name: "loaded supervisor stop fails", unit: true},
		{name: "supervisor absence cannot be confirmed", inspectErr: true},
		{name: "container survives missing supervisor", alive: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newContainerTeardownFixture(t)
			if tc.unit {
				if err := os.WriteFile(f.unit, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				f.stopErr = errors.New("stop failed")
			}
			f.containerAlive = tc.alive
			f.teardown.Run = func(ctx context.Context, log *slog.Logger, command string, args ...string) (string, error) {
				if command == "systemctl" && len(args) == 4 && args[0] == "show" && args[1] == "--property=LoadState" {
					if tc.inspectErr {
						return "", errors.New("systemd unavailable")
					}
					if tc.unit {
						return "loaded", nil
					}
					return "not-found", nil
				}
				return f.run(ctx, log, command, args...)
			}
			plan := stagedOCIReset(t, f)
			if err := phases.Serial(f.teardown.Log, plan.Stop(), plan.Finish()).Do(t.Context()); err == nil {
				t.Fatal("reset succeeded despite stop failure")
			}
			for _, path := range []string{f.snapshot, f.sharedConfig, f.teardown.Layout.RootfsDir("kube1"), filepath.Join(f.snapshot, "credentials")} {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("failed stop removed %s: %v", path, err)
				}
			}
		})
	}
}
