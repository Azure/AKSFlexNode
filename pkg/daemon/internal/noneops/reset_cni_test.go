package noneops

import (
	"path/filepath"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/daemon/internal/hostnode"
)

func TestResetRemovesIPAMReservations(t *testing.T) {
	t.Parallel()
	f := newNodeFixture(t, "1.37.0")
	if err := f.node.Provision(t.Context()); err != nil {
		t.Fatal(err)
	}
	lease := filepath.Join(f.root, hostnode.CNIStateDir, "bridge", "10.185.0.2")
	mustWrite(t, lease, "ended-pod")
	td := &Teardown{
		Log: f.node.p.Log, Root: f.root, Layout: f.node.p.Layout,
		SystemdDir: f.units, Run: f.host.run, Proc: filepath.Join(f.root, "proc"),
		MountInfo: filepath.Join(f.root, "mountinfo"),
	}
	for range 2 {
		if err := td.Destroy().Do(t.Context()); err != nil {
			t.Fatal(err)
		}
		assertExists(t, lease, false)
	}
}
