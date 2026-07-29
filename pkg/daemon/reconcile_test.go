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
	appliedGoal := goal
	applied := &State{AppliedGoal: &appliedGoal}
	staleGoal := aksmachine.GoalState{KubernetesVersion: "1.33.0", SettingsVersion: "41"}
	stale := &State{AppliedGoal: &staleGoal}
	node := nodeSnapshot{node: &corev1.Node{}}
	missingNode := nodeSnapshot{}
	deleteNode := nodeSnapshot{node: &corev1.Node{Spec: corev1.NodeSpec{Taints: []corev1.Taint{deletionTaint()}}}}
	inPlaceAppliedGoal := aksmachine.GoalState{
		KubernetesVersion: "1.34.0",
		SettingsVersion:   "41",
		NodeLabels:        map[string]string{"workload": "old", "removed": "true"},
		NodeTaints:        []string{"dedicated=old:NoSchedule", "removed=true:NoExecute"},
	}
	inPlaceGoal := aksmachine.GoalState{
		KubernetesVersion: "1.34.0",
		SettingsVersion:   "42",
		NodeLabels:        map[string]string{"workload": "new"},
		NodeTaints:        []string{"dedicated=new:NoSchedule"},
	}
	inPlaceMachine := machineSnapshot{machine: &aksmachine.Machine{Goal: inPlaceGoal}}
	inPlaceState := &State{AppliedGoal: &inPlaceAppliedGoal}
	inPlaceNode := nodeSnapshot{node: &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
			"workload":                "new",
			"unrelated.example/label": "preserved",
		}},
		Spec: corev1.NodeSpec{Taints: []corev1.Taint{
			{Key: "dedicated", Value: "new", Effect: corev1.TaintEffectNoSchedule},
			{Key: "unrelated.example/taint", Effect: corev1.TaintEffectNoExecute},
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

func TestGoalReflectedOnNode(t *testing.T) {
	t.Parallel()

	appliedGoal := aksmachine.GoalState{
		KubernetesVersion: "1.34.0",
		SettingsVersion:   "41",
		MaxPods:           110,
		NodeLabels:        map[string]string{"workload": "old", "removed": "true"},
		NodeTaints:        []string{"dedicated=old:NoSchedule", "removed=true:NoExecute"},
		KubeletConfig:     aksmachine.KubeletConfig{ImageGCHighThreshold: 85, ImageGCLowThreshold: 80},
	}
	desiredGoal := aksmachine.GoalState{
		KubernetesVersion: "1.34.0",
		SettingsVersion:   "42",
		MaxPods:           110,
		NodeLabels:        map[string]string{"workload": "new"},
		NodeTaints:        []string{"dedicated=new:NoSchedule"},
		KubeletConfig:     appliedGoal.KubeletConfig,
	}
	state := &State{AppliedGoal: &appliedGoal}
	matchingNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
			"workload":                "new",
			"unrelated.example/label": "preserved",
		}},
		Spec: corev1.NodeSpec{Taints: []corev1.Taint{
			{Key: "dedicated", Value: "new", Effect: corev1.TaintEffectNoSchedule},
			{Key: "unrelated.example/taint", Effect: corev1.TaintEffectNoExecute},
		}},
	}

	if !goalReflectedOnNode(desiredGoal, state, matchingNode) {
		t.Fatal("goalReflectedOnNode returned false for matching in-place settings")
	}

	tests := map[string]struct {
		mutateGoal  func(*aksmachine.GoalState)
		mutateState func(*State)
		mutateNode  func(*corev1.Node)
	}{
		"Kubernetes version changed": {
			mutateGoal: func(goal *aksmachine.GoalState) { goal.KubernetesVersion = "1.35.0" },
		},
		"max pods changed": {
			mutateGoal: func(goal *aksmachine.GoalState) { goal.MaxPods = 50 },
		},
		"kubelet config changed": {
			mutateGoal: func(goal *aksmachine.GoalState) { goal.KubeletConfig.ImageGCHighThreshold = 70 },
		},
		"applied goal unavailable": {
			mutateState: func(state *State) { state.AppliedGoal = nil },
		},
		"desired label missing": {
			mutateNode: func(node *corev1.Node) { delete(node.Labels, "workload") },
		},
		"desired empty label missing": {
			mutateGoal: func(goal *aksmachine.GoalState) { goal.NodeLabels = map[string]string{"empty": ""} },
			mutateNode: func(node *corev1.Node) { delete(node.Labels, "empty") },
		},
		"removed label remains": {
			mutateNode: func(node *corev1.Node) { node.Labels["removed"] = "true" },
		},
		"desired taint missing": {
			mutateNode: func(node *corev1.Node) { node.Spec.Taints = node.Spec.Taints[1:] },
		},
		"removed taint remains": {
			mutateNode: func(node *corev1.Node) {
				node.Spec.Taints = append(node.Spec.Taints, corev1.Taint{Key: "removed", Value: "true", Effect: corev1.TaintEffectNoExecute})
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			goal := desiredGoal
			stateCopy := *state
			appliedGoalCopy := appliedGoal
			stateCopy.AppliedGoal = &appliedGoalCopy
			node := matchingNode.DeepCopy()
			if tt.mutateGoal != nil {
				tt.mutateGoal(&goal)
			}
			if tt.mutateState != nil {
				tt.mutateState(&stateCopy)
			}
			if tt.mutateNode != nil {
				tt.mutateNode(node)
			}
			if goalReflectedOnNode(goal, &stateCopy, node) {
				t.Fatal("goalReflectedOnNode returned true")
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
