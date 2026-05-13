package daemon

import (
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
)

const ResetAnnotationKey = "kubernetes.azure.com/flex-node-reset"

type DecisionKind string

const (
	DecisionNoop                 DecisionKind = "Noop"
	DecisionApplyGoalState       DecisionKind = "ApplyGoalState"
	DecisionResetDelete          DecisionKind = "ResetDelete"
	DecisionWaitForMachineDelete DecisionKind = "WaitForMachineDelete"
	DecisionWaitForNodeSignal    DecisionKind = "WaitForNodeSignal"
	DecisionReportSucceeded      DecisionKind = "ReportSucceeded"
)

type machineSnapshot struct {
	machine  *aksmachine.Machine
	notFound bool
}

type nodeSnapshot struct {
	node *corev1.Node
}

type Decision struct {
	Kind   DecisionKind
	Goal   aksmachine.GoalState
	Reason string
}

func decide(machine machineSnapshot, node nodeSnapshot, state *State) Decision {
	nodeExists := node.node != nil
	resetRequested := nodeExists && annotationTrue(node.node.Annotations, ResetAnnotationKey)
	if resetRequested {
		if machine.notFound {
			return Decision{Kind: DecisionResetDelete, Reason: "reset annotation present and machine resource is gone"}
		}
		return Decision{Kind: DecisionWaitForMachineDelete, Reason: "reset annotation present but machine resource still exists"}
	}

	if machine.notFound {
		return Decision{Kind: DecisionWaitForNodeSignal, Reason: "machine resource is gone without reset annotation"}
	}
	if machine.machine == nil {
		return Decision{Kind: DecisionNoop, Reason: "machine snapshot is empty"}
	}

	goal := machine.machine.Goal
	if !nodeExists {
		if goalApplied(goal, state) {
			return Decision{Kind: DecisionReportSucceeded, Goal: goal, Reason: "node is absent but goal state is already applied"}
		}
		return Decision{Kind: DecisionApplyGoalState, Goal: goal, Reason: "node deletion observed and goal state is not applied"}
	}

	if goalApplied(goal, state) {
		return Decision{Kind: DecisionReportSucceeded, Goal: goal, Reason: "goal state is applied"}
	}

	return Decision{Kind: DecisionWaitForNodeSignal, Goal: goal, Reason: "goal state differs but node deletion trigger is absent"}
}

func goalApplied(goal aksmachine.GoalState, state *State) bool {
	if state == nil {
		return false
	}
	return goal.SettingsVersion != "" && state.AppliedSettingsVersion == goal.SettingsVersion
}

func annotationTrue(annotations map[string]string, key string) bool {
	if annotations == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(annotations[key]), "true")
}
