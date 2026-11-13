package noneops

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/daemon/internal/hostnode"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

func TestUnchangedConfigRecoveryRequiresAppliedGeneration(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, requested, applied string
		rollback                 bool
	}{
		{name: "unpersisted requested generation", requested: "43", applied: "42", rollback: true},
		{name: "persisted requested generation", requested: "43", applied: "43"},
		{name: "missing applied generation", requested: "43", rollback: true},
		{name: "missing journal generation", applied: "42", rollback: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newNodeFixture(t, "1.34.1")
			if err := f.node.Provision(t.Context()); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(f.root, none.ContainerdConfigPath)
			mustWrite(t, path, "previous config")
			if _, err := hostnode.TakeSnapshotOf(f.node.p.Log, f.node.configSnapshotDir(), f.root, configSnapshotPaths()); err != nil {
				t.Fatal(err)
			}
			// No startup marker exists: this models interrupted preparation, not
			// an exposed target whose workloads must survive recovery.
			if err := f.node.writeJournal(upgradeJournal{Previous: f.node.version, Target: f.node.version,
				TargetConfig: configDigest(f.node.p.Config), TargetGeneration: tc.requested}); err != nil {
				t.Fatal(err)
			}
			mustWrite(t, path, "prepared config")
			p := f.node.p
			p.Generation = tc.applied
			recovered := New(p)
			if err := recovered.RecoverUpgrade(t.Context()); err != nil {
				t.Fatal(err)
			}
			want := "prepared config"
			if tc.rollback {
				want = "previous config"
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != want {
				t.Fatalf("configuration = %q, want %q; error = %v", data, want, err)
			}
			if err := recovered.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := recovered.ClearStartup(); err != nil {
				t.Fatal(err)
			}
			assertExists(t, recovered.journalPath(), false)
			assertExists(t, recovered.configSnapshotDir(), false)
			if err := recovered.RecoverUpgrade(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
