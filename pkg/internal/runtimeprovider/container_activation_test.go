package runtimeprovider

import (
	"context"
	"log/slog"
	"slices"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestContainerStartupFencesStoppedSource(t *testing.T) {
	t.Parallel()
	op, events, store := containerOperatorFixture(t, "npd", false)
	if _, err := op.ApplyGoalState(t.Context(), discardLogger(), aksmachine.GoalState{SettingsVersion: "next", KubernetesVersion: "1.36.2"}); err == nil {
		t.Fatal("expected NPD failure after target startup")
	}
	startup, err := op.startup.Load()
	if err != nil || startup == nil || startup.Phase != "starting" || startup.Next.ActiveMachine != "kube2" || store.state.ActiveMachine != "kube1" {
		t.Fatalf("startup = %+v, applied = %+v, error = %v", startup, store.state, err)
	}
	if !slices.Contains(*events, "source-stop") || !slices.Contains(*events, "start") {
		t.Fatalf("did not reproduce stopped source and running target: %v", *events)
	}
	provider := containerTestProvider(t, op.startup)
	if err := provider.ValidateAgentActivation(t.Context()); err == nil {
		t.Fatal("accepted the stopped source while target completion was pending")
	}
	op.startNPD = func(ctx context.Context, log *slog.Logger, cfg *config.Config, goal *goalstates.NodeStart) error {
		return nil
	}
	if handled, err := op.ResumeRepave(t.Context(), discardLogger(), nil); !handled || err != nil {
		t.Fatalf("resume = %t, %v", handled, err)
	}
	if err := provider.ValidateAgentActivation(t.Context()); err != nil {
		t.Fatal(err)
	}
	if store.state.ActiveMachine != "kube2" {
		t.Fatal("recovery did not publish the running target")
	}
}
