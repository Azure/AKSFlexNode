package daemon

import (
	"context"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	corev1 "k8s.io/api/core/v1"
)

func TestNoneHubIntentIsReportedWithoutNSpawnOperations(t *testing.T) {
	t.Parallel()
	for _, enabled := range []bool{false, true} {
		name := "disabled"
		if enabled {
			name = "unsupported"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			adapter, err := newHubMachineAdapter(func(string) (hubTransitionOperations, error) {
				t.Fatal("none node must not operate an nspawn worker")
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			var condition corev1.NodeCondition
			adapter.setCondition = func(_ context.Context, c corev1.NodeCondition) error { condition = c; return nil }
			for range 2 {
				if err := adapter.ReconcileHub(t.Context(), aksmachine.GoalState{DisconnectToleranceEnabled: &enabled}, &State{Isolation: isolation.ModeNone}, nil); err != nil {
					t.Fatal(err)
				}
			}
			want := hubReasonDisabled
			if enabled {
				want = "Unsupported"
			}
			if condition.Reason != want {
				t.Fatalf("condition = %s, want %s", condition.Reason, want)
			}
		})
	}
}
