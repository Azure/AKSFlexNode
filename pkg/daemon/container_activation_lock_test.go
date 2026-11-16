package daemon

import (
	"context"
	"log/slog"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestContainerRepaveSharesAgentActivationLock(t *testing.T) {
	t.Parallel()
	op, events, _ := containerOperatorFixture(t, "", false)
	lock, err := agentbinary.AcquireHostActivationLock(op.activationLockPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.ApplyGoalState(t.Context(), discardLogger(), aksmachine.GoalState{SettingsVersion: "new"}); err == nil {
		t.Fatal("repave began while direct activation held the shared lock")
	}
	if len(*events) != 0 {
		t.Fatalf("blocked repave performed mutations: %v", *events)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	op.stage = func(context.Context, *slog.Logger, *goalstates.ContainerImageArchiveStaging) error {
		other, err := agentbinary.AcquireHostActivationLock(op.activationLockPath)
		if err == nil {
			if err := other.Close(); err != nil {
				t.Fatal(err)
			}
			t.Fatal("direct activation could start during repave preparation")
		}
		return nil
	}
	if _, err := op.ApplyGoalState(t.Context(), discardLogger(), aksmachine.GoalState{SettingsVersion: "new", KubernetesVersion: "1.36.2"}); err != nil {
		t.Fatal(err)
	}
	lock, err = agentbinary.AcquireHostActivationLock(op.activationLockPath)
	if err != nil {
		t.Fatalf("successful repave did not release its lock: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestContainerResumeWaitsForAgentActivationLock(t *testing.T) {
	t.Parallel()
	op, events, store := containerOperatorFixture(t, "", false)
	if err := op.startup.Save(t.Context(), &containerStartup{Phase: "starting", Next: containerTestState("kube2", "new"), Previous: store.state}); err != nil {
		t.Fatal(err)
	}
	lock, err := agentbinary.AcquireHostActivationLock(op.activationLockPath)
	if err != nil {
		t.Fatal(err)
	}
	if handled, err := op.ResumeRepave(t.Context(), discardLogger(), nil); !handled || err == nil {
		t.Fatalf("resume ignored the shared activation lock: handled=%t error=%v", handled, err)
	}
	if len(*events) != 0 {
		t.Fatalf("blocked resume performed mutations: %v", *events)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if handled, err := op.ResumeRepave(t.Context(), discardLogger(), nil); !handled || err != nil {
		t.Fatalf("resume did not recover after the lock was released: handled=%t error=%v", handled, err)
	}
}
