package noneops

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/daemon/internal/hostnode"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

func TestRollbackRestoresFlexDependencies(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, appliedGeneration string
		failStart               bool
	}{
		{name: "failed target start", appliedGeneration: "41", failStart: true},
		{name: "interrupted before persistence", appliedGeneration: "41"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newNodeFixture(t, "1.34.1")
			f.units = filepath.Join(f.root, goalstates.SystemdSystemDir)
			f.node.p.SystemdDir = f.units
			units := []string{none.UnitContainerd, none.UnitKubelet, none.UnitLocalDNS}
			dependencyPath := func(unit string) string {
				return filepath.Join(none.DropInDir(f.units, unit), "10-flex-dependencies.conf")
			}
			const previous = "[Unit]\nRequires=previous-required.service\nAfter=previous-required.service\n"
			const target = "[Unit]\nRequires=new-required.service\nAfter=new-required.service\n"
			f.node.p.Run = func(ctx context.Context, log *slog.Logger, name string, args ...string) (string, error) {
				if tc.failStart && name == "systemctl" && len(args) > 1 && args[0] == "enable" {
					data, err := os.ReadFile(dependencyPath(args[len(args)-1]))
					if err != nil {
						return "", err
					}
					if strings.Contains(string(data), "new-required.service") {
						return "", errors.New("required service failed")
					}
				}
				return f.host.run(ctx, log, name, args...)
			}
			if err := f.node.Provision(t.Context()); err != nil {
				t.Fatal(err)
			}
			for _, unit := range units {
				mustWrite(t, dependencyPath(unit), previous)
			}
			f.node.p.Install = func(context.Context, *goalstates.MachineGoalState) error {
				for _, unit := range units {
					mustWrite(t, dependencyPath(unit), target)
					mustWrite(t, filepath.Join(none.DropInDir(f.units, unit), "20-flex-extra.conf"), target)
				}
				return nil
			}
			assertRestored := func() {
				t.Helper()
				for _, unit := range units {
					data, err := os.ReadFile(dependencyPath(unit))
					if err != nil || string(data) != previous {
						t.Fatalf("%s dependencies were not restored: %q, %v", unit, data, err)
					}
					assertExists(t, filepath.Join(none.DropInDir(f.units, unit), "20-flex-extra.conf"), false)
				}
				if !f.host.active[none.UnitContainerd] || !f.host.active[none.UnitKubelet] {
					t.Fatal("rollback did not restart the previous stack")
				}
			}

			if tc.failStart {
				if err := f.node.Upgrade(t.Context()); err == nil {
					t.Fatal("target unexpectedly started with a failed dependency")
				}
				assertRestored()
			} else {
				// Crash during installation, before either target startup or
				// publication of its exposure marker.
				if _, err := hostnode.TakeSnapshotOf(f.node.p.Log, f.node.configSnapshotDir(), f.root, configSnapshotPaths()); err != nil {
					t.Fatal(err)
				}
				if err := f.node.writeJournal(upgradeJournal{Previous: f.node.version, Target: f.node.version, TargetGeneration: "43"}); err != nil {
					t.Fatal(err)
				}
				if err := f.node.p.Install(t.Context(), f.node.gs); err != nil {
					t.Fatal(err)
				}
			}
			p := f.node.p
			p.Generation = tc.appliedGeneration
			recovered := New(p)
			if err := recovered.RecoverUpgrade(t.Context()); err != nil {
				t.Fatal(err)
			}
			assertRestored()
			if err := recovered.RecoverUpgrade(t.Context()); err != nil {
				t.Fatalf("repeated recovery: %v", err)
			}
		})
	}
}
