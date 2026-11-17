package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/internal/runtimeprovider"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Reset must not depend on upgrade recovery or its health dependencies.
type resetCompletionOperator struct {
	*fakeNodeOperator
	resumed bool
}

func (o *resetCompletionOperator) ResumeRepave(ctx context.Context, log *slog.Logger, node *corev1.Node) (bool, error) {
	o.resumed = true
	return true, errors.New("recovery unavailable")
}

func TestNoneCompletionRespectsDeletePrecedence(t *testing.T) {
	t.Parallel()
	for _, removed := range []bool{false, true} {
		t.Run(fmt.Sprintf("machine removed=%t", removed), func(t *testing.T) {
			t.Parallel()
			op := &resetCompletionOperator{fakeNodeOperator: &fakeNodeOperator{state: &runtimeprovider.State{AppliedSettingsVersion: "41"}}}
			machines := &fakeMachineClient{notFound: removed, machine: &aksmachine.Machine{Goal: aksmachine.GoalState{SettingsVersion: "42", KubernetesVersion: "1.34.2"}}}
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1"}, Spec: corev1.NodeSpec{Taints: []corev1.Taint{deletionTaint()}}}
			r := newTestRepaveReconciler(t, machines, fakeClient(node), op)
			if err := r.reconcileOnce(t.Context()); err != nil {
				t.Fatal(err)
			}
			if op.resumed || op.reset != removed || op.stopped != removed {
				t.Fatal("recovery bypassed RP deletion/reset precedence")
			}
		})
	}
}
