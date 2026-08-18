package daemon

import (
	"maps"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
)

func TestDecide(t *testing.T) {
	t.Parallel()

	goal := testMachineGoal("1.34.0", "42")
	machine := machineSnapshot{machine: &aksmachine.Machine{Goal: goal}}
	applied := &State{AppliedGoal: goal.DeepCopy()}
	staleGoal := testMachineGoal("1.33.0", "41")
	stale := &State{AppliedGoal: &staleGoal}
	node := nodeSnapshot{node: &corev1.Node{}}
	missingNode := nodeSnapshot{}
	deleteNode := nodeSnapshot{node: &corev1.Node{Spec: corev1.NodeSpec{Taints: []corev1.Taint{deletionTaint()}}}}
	inPlaceAppliedGoal := testMachineGoal("1.34.0", "41")
	inPlaceAppliedGoal.NodeLabels = map[string]string{"workload": "old", "removed": "true"}
	inPlaceAppliedGoal.NodeTaints = []string{"dedicated=old:NoSchedule", "removed=true:NoExecute"}
	inPlaceGoal := testMachineGoal("1.34.0", "42")
	inPlaceGoal.NodeLabels = map[string]string{"workload": "new"}
	inPlaceGoal.NodeTaints = []string{"dedicated=new:NoSchedule"}
	inPlaceMachine := machineSnapshot{machine: &aksmachine.Machine{Goal: inPlaceGoal}}
	inPlaceState := &State{AppliedGoal: &inPlaceAppliedGoal}
	inPlaceNode := nodeSnapshot{node: &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"workload": "new"}},
		Spec: corev1.NodeSpec{Taints: []corev1.Taint{
			{Key: "dedicated", Value: "new", Effect: corev1.TaintEffectNoSchedule},
		}},
	}}

	tests := map[string]struct {
		machine machineSnapshot
		node    nodeSnapshot
		state   *State
		want    decisionKind
	}{
		"reset waits for machine delete": {
			machine: machine,
			node:    deleteNode,
			state:   applied,
			want:    decisionWaitForMachineDelete,
		},
		"reset after machine delete": {
			machine: machineSnapshot{notFound: true},
			node:    deleteNode,
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
		"present node acknowledges RP-applied labels and taints": {
			machine: inPlaceMachine,
			node:    inPlaceNode,
			state:   inPlaceState,
			want:    decisionAcknowledgeGoalState,
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

func TestGoalForInPlaceAcknowledgement(t *testing.T) {
	t.Parallel()

	newFixture := func() (aksmachine.GoalState, *State, *corev1.Node) {
		appliedGoal := testMachineGoal("1.34.0", "41")
		appliedGoal.NodeLabels = map[string]string{"workload": "old", "removed": "true"}
		appliedGoal.NodeTaints = []string{"dedicated=old:NoSchedule", "removed=true:NoExecute"}
		desiredGoal := testMachineGoal("1.34.0", "42")
		desiredGoal.NodeLabels = map[string]string{"workload": "new", "empty": ""}
		desiredGoal.NodeTaints = []string{"dedicated=new:NoSchedule", "empty:NoSchedule"}
		now := metav1.Now()
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				"workload":                           "new",
				"empty":                              "",
				"kubernetes.io/hostname":             "node1",
				"kubernetes.azure.com/managed":       "false",
				"kubernetes.azure.com/agentpool":     "flexpool",
				"kubernetes.azure.com/mode":          "user",
				"kubernetes.azure.com/nodepool-type": "FlexNodes",
			}},
			Spec: corev1.NodeSpec{Taints: []corev1.Taint{
				{Key: "unrelated.example/taint", Effect: corev1.TaintEffectNoExecute},
				{Key: "empty", Effect: corev1.TaintEffectNoSchedule, TimeAdded: &now},
				{Key: "dedicated", Value: "new", Effect: corev1.TaintEffectNoSchedule},
			}},
		}
		return desiredGoal, &State{AppliedGoal: &appliedGoal}, node
	}

	tests := map[string]struct {
		mutate      func(*aksmachine.GoalState, *State, *corev1.Node)
		want        bool
		wantMaxPods int
	}{
		"reflected label and taint delta": {want: true, wantMaxPods: 110},
		"omitted scalar defaults are preserved": {
			mutate: func(goal *aksmachine.GoalState, _ *State, _ *corev1.Node) {
				goal.MaxPods = nil
				goal.KubeletConfig = aksmachine.KubeletConfig{}
			},
			want:        true,
			wantMaxPods: 110,
		},
		"nil labels and taints clear applied values": {
			mutate: func(goal *aksmachine.GoalState, _ *State, node *corev1.Node) {
				goal.NodeLabels = nil
				goal.NodeTaints = nil
				delete(node.Labels, "workload")
				delete(node.Labels, "empty")
				node.Spec.Taints = node.Spec.Taints[:1]
			},
			want:        true,
			wantMaxPods: 110,
		},
		"empty labels and taints clear applied values": {
			mutate: func(goal *aksmachine.GoalState, _ *State, node *corev1.Node) {
				goal.NodeLabels = map[string]string{}
				goal.NodeTaints = []string{}
				delete(node.Labels, "workload")
				delete(node.Labels, "empty")
				node.Spec.Taints = node.Spec.Taints[:1]
			},
			want:        true,
			wantMaxPods: 110,
		},
		"Kubernetes version changed": {
			mutate: func(goal *aksmachine.GoalState, _ *State, _ *corev1.Node) { goal.KubernetesVersion = "1.35.0" },
		},
		"max pods changed": {
			mutate: func(goal *aksmachine.GoalState, _ *State, _ *corev1.Node) { goal.MaxPods = intPointer(50) },
		},
		"image GC high threshold changed": {
			mutate: func(goal *aksmachine.GoalState, _ *State, _ *corev1.Node) {
				goal.KubeletConfig.ImageGCHighThreshold = intPointer(90)
			},
		},
		"image GC low threshold changed": {
			mutate: func(goal *aksmachine.GoalState, _ *State, _ *corev1.Node) {
				goal.KubeletConfig.ImageGCLowThreshold = intPointer(75)
			},
		},
		"applied goal unavailable": {
			mutate: func(_ *aksmachine.GoalState, state *State, _ *corev1.Node) { state.AppliedGoal = nil },
		},
		"desired label missing": {
			mutate: func(_ *aksmachine.GoalState, _ *State, node *corev1.Node) { delete(node.Labels, "workload") },
		},
		"desired label has wrong value": {
			mutate: func(_ *aksmachine.GoalState, _ *State, node *corev1.Node) { node.Labels["workload"] = "old" },
		},
		"desired empty label missing": {
			mutate: func(_ *aksmachine.GoalState, _ *State, node *corev1.Node) { delete(node.Labels, "empty") },
		},
		"removed label remains with another value": {
			mutate: func(_ *aksmachine.GoalState, _ *State, node *corev1.Node) { node.Labels["removed"] = "changed" },
		},
		"desired taint missing": {
			mutate: func(_ *aksmachine.GoalState, _ *State, node *corev1.Node) {
				node.Spec.Taints = node.Spec.Taints[:2]
			},
		},
		"desired taint has wrong value": {
			mutate: func(_ *aksmachine.GoalState, _ *State, node *corev1.Node) {
				node.Spec.Taints[2].Value = "old"
			},
		},
		"updated taint key retains another effect": {
			mutate: func(_ *aksmachine.GoalState, _ *State, node *corev1.Node) {
				node.Spec.Taints = append(node.Spec.Taints, corev1.Taint{Key: "dedicated", Value: "stale", Effect: corev1.TaintEffectNoExecute})
			},
		},
		"removed taint identity remains": {
			mutate: func(_ *aksmachine.GoalState, _ *State, node *corev1.Node) {
				node.Spec.Taints = append(node.Spec.Taints, corev1.Taint{Key: "removed", Value: "changed", Effect: corev1.TaintEffectNoExecute})
			},
		},
		"removed taint key remains with another effect": {
			mutate: func(_ *aksmachine.GoalState, _ *State, node *corev1.Node) {
				node.Spec.Taints = append(node.Spec.Taints, corev1.Taint{Key: "removed", Value: "changed", Effect: corev1.TaintEffectNoSchedule})
			},
		},
		"malformed desired taint": {
			mutate: func(goal *aksmachine.GoalState, _ *State, _ *corev1.Node) {
				goal.NodeTaints = append(goal.NodeTaints, "malformed")
			},
		},
		"duplicate desired taint identity": {
			mutate: func(goal *aksmachine.GoalState, _ *State, _ *corev1.Node) {
				goal.NodeTaints = append(goal.NodeTaints, "dedicated=duplicate:NoSchedule")
			},
		},
		"node is deleting": {
			mutate: func(_ *aksmachine.GoalState, _ *State, node *corev1.Node) {
				now := metav1.Now()
				node.DeletionTimestamp = &now
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			goal, state, node := newFixture()
			if tt.mutate != nil {
				tt.mutate(&goal, state, node)
			}
			got, ok := goalForInPlaceAcknowledgement(goal, state, node)
			if ok != tt.want {
				t.Fatalf("goalForInPlaceAcknowledgement() ok = %v, want %v", ok, tt.want)
			}
			if ok {
				if got.MaxPods == nil || *got.MaxPods != tt.wantMaxPods || !maps.Equal(got.NodeLabels, goal.NodeLabels) {
					t.Fatalf("acknowledged goal = %#v", got)
				}
			}
		})
	}
}

func TestHasDeletionSignal(t *testing.T) {
	t.Parallel()

	if !hasDeletionSignal([]corev1.Taint{deletionTaint()}) {
		t.Fatal("hasDeletionSignal returned false")
	}
	if hasDeletionSignal([]corev1.Taint{{Key: DeletionTaintKey, Value: "false", Effect: DeletionTaintEffect}}) {
		t.Fatal("hasDeletionSignal returned true for wrong value")
	}
	if hasDeletionSignal([]corev1.Taint{{Key: DeletionTaintKey, Value: DeletionTaintValue, Effect: corev1.TaintEffectNoExecute}}) {
		t.Fatal("hasDeletionSignal returned true for wrong effect")
	}
}

func deletionTaint() corev1.Taint {
	return corev1.Taint{Key: DeletionTaintKey, Value: DeletionTaintValue, Effect: DeletionTaintEffect}
}
