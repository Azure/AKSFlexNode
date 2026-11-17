package daemon

import (
	"context"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	corev1 "k8s.io/api/core/v1"
)

func TestContainerHubIntentDoesNotOperateNSpawn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		enabled bool
		reason  string
	}{
		{name: "disabled", reason: hubReasonDisabled}, {name: "unsupported", enabled: true, reason: "Unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			adapter, err := newHubMachineAdapter(func(string) (hubTransitionOperations, error) {
				t.Fatal("container DT invoked nspawn operations")
				return nil, nil
			}, mustNodeProvider(t, isolation.ModeContainer))
			if err != nil {
				t.Fatal(err)
			}
			var condition corev1.NodeCondition
			adapter.setCondition = func(_ context.Context, c corev1.NodeCondition) error { condition = c; return nil }
			for range 2 {
				if err := adapter.ReconcileHub(t.Context(), aksmachine.GoalState{DisconnectToleranceEnabled: &tc.enabled}, containerTestState("kube1", "old"), nil); err != nil {
					t.Fatal(err)
				}
			}
			if condition.Reason != tc.reason {
				t.Fatalf("condition = %s, want %s", condition.Reason, tc.reason)
			}
		})
	}
}
