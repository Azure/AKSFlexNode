package runtimeprovider

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
)

func TestContainerCompletionSyncFailureKeepsStartupAndSource(t *testing.T) {
	t.Parallel()
	op, events, store := containerOperatorFixture(t, "", false)
	syncErr := errors.New("post-start boot state did not persist")
	calls := 0
	op.syncPrepared = func(ctx context.Context, node ContainerRuntime) error {
		calls++
		if calls == 2 {
			return syncErr
		}
		return nil
	}
	_, err := op.ApplyGoalState(t.Context(), discardLogger(), aksmachine.GoalState{SettingsVersion: "new", KubernetesVersion: "1.36.2"})
	if !errors.Is(err, syncErr) {
		t.Fatalf("completion ignored persistence failure: %v", err)
	}
	startup, err := op.startup.Load()
	if err != nil || startup == nil || startup.Phase != "starting" {
		t.Fatalf("published startup was cleared on failed persistence: %+v %v", startup, err)
	}
	if store.state.ActiveMachine != "kube1" || slices.Contains(*events, "destroy-kube1") {
		t.Fatalf("failed completion retired source or published target: %v", *events)
	}
}
