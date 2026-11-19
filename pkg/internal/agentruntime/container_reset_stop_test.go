package agentruntime

import (
	"context"
	"errors"
	"os"
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

	return discoverReset(f.teardown.Log, nspawn, native, &ociTeardown{Teardown: f.teardown}, shared)
}

func TestOCIStagedResetReturnsMissingSupervisorError(t *testing.T) {
	t.Parallel()

	f := newContainerTeardownFixture(t)
	plan := stagedOCIReset(t, f)

	err := phases.Serial(f.teardown.Log, plan.Stop(), plan.Finish()).Do(t.Context())
	if err == nil || !strings.Contains(err.Error(), "unit not loaded") {
		t.Fatalf("reset error = %v, want SDK stop failure", err)
	}
	for _, path := range []string{f.modules, f.sharedConfig, f.teardown.Layout.RootfsDir("kube1")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("failed stop removed %s: %v", path, err)
		}
	}
}

func TestOCIStagedStopPreservesRealFailures(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		unit  bool
		alive bool
	}{
		{name: "loaded supervisor stop fails", unit: true},
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
			plan := stagedOCIReset(t, f)

			if err := phases.Serial(f.teardown.Log, plan.Stop(), plan.Finish()).Do(t.Context()); err == nil {
				t.Fatal("reset succeeded despite SDK stop failure")
			}
			for _, path := range []string{f.modules, f.sharedConfig, f.teardown.Layout.RootfsDir("kube1")} {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("failed stop removed %s: %v", path, err)
				}
			}
		})
	}
}
