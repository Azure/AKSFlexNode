package daemon

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
)

func TestDecide(t *testing.T) {
	t.Parallel()

	goal := aksmachine.GoalState{KubernetesVersion: "1.34.0", SettingsVersion: "42"}
	machine := machineSnapshot{machine: &aksmachine.Machine{Goal: goal}}
	applied := &State{AppliedSettingsVersion: "42", AppliedKubernetesVersion: "1.34.0"}
	stale := &State{AppliedSettingsVersion: "41", AppliedKubernetesVersion: "1.33.0"}
	node := nodeSnapshot{node: &corev1.Node{}}
	missingNode := nodeSnapshot{}
	resetNode := nodeSnapshot{node: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{ResetAnnotationKey: "true"}}}}

	tests := map[string]struct {
		machine machineSnapshot
		node    nodeSnapshot
		state   *State
		want    decisionKind
	}{
		"reset waits for machine delete": {
			machine: machine,
			node:    resetNode,
			state:   applied,
			want:    decisionWaitForMachineDelete,
		},
		"reset after machine delete": {
			machine: machineSnapshot{notFound: true},
			node:    resetNode,
			state:   applied,
			want:    decisionResetDelete,
		},
		"machine not found without reset waits": {
			machine: machineSnapshot{notFound: true},
			node:    node,
			state:   applied,
			want:    decisionWaitForNodeSignal,
		},
		"node deletion applies unapplied goal": {
			machine: machine,
			node:    missingNode,
			state:   stale,
			want:    decisionApplyGoalState,
		},
		"node deletion reports applied goal": {
			machine: machine,
			node:    missingNode,
			state:   applied,
			want:    decisionReportSucceeded,
		},
		"present node reports applied goal": {
			machine: machine,
			node:    node,
			state:   applied,
			want:    decisionReportSucceeded,
		},
		"present node waits for deletion before applying drift": {
			machine: machine,
			node:    node,
			state:   stale,
			want:    decisionWaitForNodeSignal,
		},
		"present node waits with missing state": {
			machine: machine,
			node:    node,
			state:   nil,
			want:    decisionWaitForNodeSignal,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := decide(tt.machine, tt.node, tt.state)
			if got.Kind != tt.want {
				t.Fatalf("decision = %s, want %s", got.Kind, tt.want)
			}
		})
	}
}

func TestAnnotationTrue(t *testing.T) {
	t.Parallel()

	if !annotationTrue(map[string]string{ResetAnnotationKey: " TRUE "}, ResetAnnotationKey) {
		t.Fatal("annotationTrue returned false")
	}
	if annotationTrue(map[string]string{ResetAnnotationKey: "yes"}, ResetAnnotationKey) {
		t.Fatal("annotationTrue returned true")
	}
}
