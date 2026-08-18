package daemon

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
)

func TestRepaveReconcilerApplyGoalState(t *testing.T) {
	t.Parallel()

	goal := testMachineGoal("1.34.0", "42")
	previousGoal := testMachineGoal("1.33.0", "41")
	machines := &fakeMachineClient{machine: &aksmachine.Machine{Goal: goal}}
	operator := &fakeNodeOperator{
		state: &State{AppliedGoal: &previousGoal, ActiveMachine: "kube1"},
		newState: &State{
			AppliedGoal:         &goal,
			PreviousAppliedGoal: &previousGoal,
			ActiveMachine:       "kube2",
		},
	}
	repaves := newTestRepaveReconciler(t, machines, fakeClient(), operator)

	if err := repaves.reconcileOnce(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !operator.applied {
		t.Fatal("ApplyGoalState was not called")
	}
	if stateObservedVersion(operator.state) != "42" || operator.state.PreviousAppliedGoal == nil || operator.state.PreviousAppliedGoal.SettingsVersion != "41" || operator.state.ActiveMachine != "kube2" {
		t.Fatalf("state = %#v", operator.state)
	}
	if got := machines.status.ProvisioningState; got != aksmachine.ProvisioningStateSucceeded {
		t.Fatalf("status = %s", got)
	}
}

func TestRepaveReconcilerAcknowledgesInPlaceGoalState(t *testing.T) {
	t.Parallel()

	appliedGoal := testMachineGoal("1.34.0", "41")
	appliedGoal.NodeLabels = map[string]string{"workload": "old"}
	appliedGoal.NodeTaints = []string{"dedicated=old:NoSchedule"}
	desiredGoal := testMachineGoal("1.34.0", "42")
	desiredGoal.NodeLabels = map[string]string{"workload": "new"}
	desiredGoal.NodeTaints = []string{"dedicated=new:NoSchedule"}
	machines := &fakeMachineClient{machine: &aksmachine.Machine{Goal: desiredGoal}}
	operator := &fakeNodeOperator{state: &State{AppliedGoal: &appliedGoal, ActiveMachine: "kube1"}}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: map[string]string{"workload": "new"}},
		Spec: corev1.NodeSpec{Taints: []corev1.Taint{
			{Key: "dedicated", Value: "new", Effect: corev1.TaintEffectNoSchedule},
		}},
	}
	repaves := newTestRepaveReconciler(t, machines, fakeClient(node), operator)

	if err := repaves.reconcileOnce(t.Context()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !operator.acknowledged {
		t.Fatal("AcknowledgeGoalState was not called")
	}
	if operator.applied || operator.restarted || operator.reset || operator.stopped {
		t.Fatalf("host mutation called: %#v", operator)
	}
	if stateObservedVersion(operator.state) != "42" || operator.state.ActiveMachine != "kube1" {
		t.Fatalf("state = %#v", operator.state)
	}
	if operator.state.PreviousAppliedGoal == nil || operator.state.PreviousAppliedGoal.SettingsVersion != "41" {
		t.Fatalf("PreviousAppliedGoal = %#v, want settings version 41", operator.state.PreviousAppliedGoal)
	}
	if got := machines.status.ObservedSettingsVersion; got != "42" {
		t.Fatalf("observed settings version = %q, want 42", got)
	}
	if got := machines.status.ProvisioningState; got != aksmachine.ProvisioningStateSucceeded {
		t.Fatalf("status = %s", got)
	}

	operator.acknowledged = false
	if err := repaves.reconcileOnce(t.Context()); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if operator.acknowledged {
		t.Fatal("AcknowledgeGoalState was called again for an applied ETag")
	}
	if operator.state.PreviousAppliedGoal == nil || operator.state.PreviousAppliedGoal.SettingsVersion != "41" {
		t.Fatalf("second reconcile rotated PreviousAppliedGoal: %#v", operator.state.PreviousAppliedGoal)
	}
}

func TestRepaveReconcilerRetriesStatusAfterAcknowledgement(t *testing.T) {
	t.Parallel()

	appliedGoal := testMachineGoal("1.34.0", "41")
	appliedGoal.NodeLabels = map[string]string{"workload": "old"}
	desiredGoal := testMachineGoal("1.34.0", "42")
	desiredGoal.NodeLabels = map[string]string{"workload": "new"}
	statusErr := errors.New("patch status")
	machines := &fakeMachineClient{
		machine:  &aksmachine.Machine{Goal: desiredGoal},
		patchErr: statusErr,
	}
	operator := &fakeNodeOperator{state: &State{AppliedGoal: &appliedGoal, ActiveMachine: "kube1"}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: map[string]string{"workload": "new"}}}
	repaves := newTestRepaveReconciler(t, machines, fakeClient(node), operator)

	if err := repaves.reconcileOnce(t.Context()); !errors.Is(err, statusErr) {
		t.Fatalf("first Reconcile error = %v, want status failure", err)
	}
	if !operator.acknowledged || stateObservedVersion(operator.state) != "42" {
		t.Fatalf("acknowledged state = %#v", operator.state)
	}

	operator.acknowledged = false
	machines.patchErr = nil
	if err := repaves.reconcileOnce(t.Context()); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if operator.acknowledged {
		t.Fatal("second reconcile acknowledged the same ETag again")
	}
	if operator.state.PreviousAppliedGoal == nil || operator.state.PreviousAppliedGoal.SettingsVersion != "41" {
		t.Fatalf("second reconcile rotated PreviousAppliedGoal: %#v", operator.state.PreviousAppliedGoal)
	}
	if got := machines.status.ObservedSettingsVersion; got != "42" {
		t.Fatalf("observed settings version = %q, want 42", got)
	}
}

func TestRepaveReconcilerAcknowledgementFailurePatchesFailed(t *testing.T) {
	t.Parallel()

	appliedGoal := testMachineGoal("1.34.0", "41")
	appliedGoal.NodeLabels = map[string]string{"workload": "old"}
	desiredGoal := testMachineGoal("1.34.0", "42")
	desiredGoal.NodeLabels = map[string]string{"workload": "new"}
	machines := &fakeMachineClient{machine: &aksmachine.Machine{Goal: desiredGoal}}
	ackErr := errors.New("save acknowledgement")
	operator := &fakeNodeOperator{
		state:  &State{AppliedGoal: &appliedGoal, ActiveMachine: "kube1"},
		ackErr: ackErr,
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: map[string]string{"workload": "new"}}}
	repaves := newTestRepaveReconciler(t, machines, fakeClient(node), operator)

	err := repaves.reconcileOnce(t.Context())
	if !errors.Is(err, ackErr) {
		t.Fatalf("Reconcile error = %v, want acknowledgement failure", err)
	}
	if got := machines.status.ProvisioningState; got != aksmachine.ProvisioningStateFailed {
		t.Fatalf("status = %s, want Failed", got)
	}
	if got := machines.status.ObservedSettingsVersion; got != "41" {
		t.Fatalf("observed settings version = %q, want 41", got)
	}
}

func TestRepaveReconcilerResetDelete(t *testing.T) {
	t.Parallel()

	machines := &fakeMachineClient{notFound: true}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1"}, Spec: corev1.NodeSpec{Taints: []corev1.Taint{deletionTaint()}}}
	operator := &fakeNodeOperator{}
	kubeClient := fakeClient(node)
	repaves := newTestRepaveReconciler(t, machines, kubeClient, operator)

	if err := repaves.reconcileOnce(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !operator.reset {
		t.Fatal("ResetNode was not called")
	}
	if !operator.stopped {
		t.Fatal("StopDaemon was not called")
	}
	var got corev1.Node
	if err := kubeClient.Get(context.Background(), client.ObjectKey{Name: "node1"}, &got); err != nil {
		t.Fatalf("node should remain after daemon reset-delete: %v", err)
	}
}

func TestRepaveReconcilerStateLoadFailurePatchesFailed(t *testing.T) {
	t.Parallel()

	machines := &fakeMachineClient{machine: &aksmachine.Machine{}}
	repaves := newTestRepaveReconciler(t, machines, fakeClient(), &fakeNodeOperator{err: errors.New("bad state")})

	if err := repaves.reconcileOnce(context.Background()); err == nil {
		t.Fatal("Reconcile error = nil, want error")
	}
	if got := machines.status.ProvisioningState; got != aksmachine.ProvisioningStateFailed {
		t.Fatalf("status = %s", got)
	}
}

func TestRepaveReconcilerRejectsInvalidMachineGoal(t *testing.T) {
	t.Parallel()

	machines := &fakeMachineClient{machine: &aksmachine.Machine{Goal: aksmachine.GoalState{KubernetesVersion: "1.34.0"}}}
	operator := &fakeNodeOperator{state: &State{AppliedSettingsVersion: "41", AppliedKubernetesVersion: "1.33.0", ActiveMachine: "kube1"}}
	repaves := newTestRepaveReconciler(t, machines, fakeClient(), operator)

	err := repaves.reconcileOnce(t.Context())
	if err == nil || !strings.Contains(err.Error(), "validate AKS machine snapshot") {
		t.Fatalf("Reconcile error = %v, want invalid machine snapshot", err)
	}
	if operator.applied {
		t.Fatal("ApplyGoalState was called for an invalid machine goal")
	}
}

func newTestRepaveReconciler(t *testing.T, machines aksmachine.MachineClient, kubeClient client.Client, operator nodeOperator) *repaveReconciler {
	t.Helper()
	repaves, err := newRepaveReconciler(repaveReconcilerOptions{
		Log:      slog.Default(),
		Machines: machines,
		Client:   kubeClient,
		Operator: operator,
		NodeName: "node1",
	})
	if err != nil {
		t.Fatalf("newRepaveReconciler: %v", err)
	}
	return repaves
}

func fakeClient(objects ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(objects...).Build()
}

type fakeMachineClient struct {
	machine  *aksmachine.Machine
	status   aksmachine.Status
	patchErr error
	notFound bool
}

func (f *fakeMachineClient) Create(context.Context, aksmachine.GoalState) (*aksmachine.Machine, error) {
	return f.machine, nil
}

func (f *fakeMachineClient) Get(context.Context) (*aksmachine.Machine, error) {
	if f.notFound {
		return nil, &aksmachine.NotFoundError{Resource: "machine"}
	}
	return f.machine, nil
}

func (f *fakeMachineClient) PatchStatus(_ context.Context, status aksmachine.Status) error {
	f.status = status
	return f.patchErr
}

type fakeNodeOperator struct {
	state        *State
	newState     *State
	err          error
	ackErr       error
	restartErr   error
	resetErr     error
	stopErr      error
	applied      bool
	acknowledged bool
	restarted    bool
	reset        bool
	stopped      bool
}

func (f *fakeNodeOperator) LoadState(context.Context) (*State, error) {
	return f.state, f.err
}

func (f *fakeNodeOperator) ApplyGoalState(context.Context, *slog.Logger, aksmachine.GoalState) (*State, error) {
	f.applied = true
	if f.newState != nil {
		f.state = f.newState
		return f.newState, nil
	}
	return f.state, nil
}

func (f *fakeNodeOperator) AcknowledgeGoalState(_ context.Context, goal aksmachine.GoalState) (*State, error) {
	f.acknowledged = true
	if f.ackErr != nil {
		return nil, f.ackErr
	}
	activeMachineName := ""
	if f.state != nil {
		activeMachineName = f.state.ActiveMachine
	}
	f.state = nextAppliedState(f.state, goal, &activeMachine{Name: activeMachineName})
	return f.state, nil
}

func (f *fakeNodeOperator) RestartNode(context.Context, *slog.Logger) error {
	f.restarted = true
	return f.restartErr
}

func (f *fakeNodeOperator) ResetNode(context.Context, *slog.Logger) error {
	f.reset = true
	f.state = nil
	return f.resetErr
}

func (f *fakeNodeOperator) StopDaemon(context.Context, *slog.Logger) error {
	f.stopped = true
	return f.stopErr
}
