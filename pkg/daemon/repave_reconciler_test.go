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
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/internal/agentruntime"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

func TestRepaveReconcilerApplyGoalState(t *testing.T) {
	t.Parallel()

	machines := &fakeMachineClient{machine: &aksmachine.Machine{Goal: aksmachine.GoalState{KubernetesVersion: "1.34.0", SettingsVersion: "42"}}}
	operator := &fakeWorkerRuntime{
		state: &State{
			Isolation:                config.DefaultIsolationMode,
			AppliedSettingsVersion:   "41",
			AppliedKubernetesVersion: "1.33.0",
			ActiveMachine:            "kube1",
		},
	}
	repaves := newTestRepaveReconciler(t, machines, fakeClient(), operator)

	if err := repaves.reconcileOnce(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !operator.applied {
		t.Fatal("ApplyGoalState was not called")
	}
	if operator.state.AppliedSettingsVersion != "42" || operator.state.PreviousSettingsVersion != "41" || operator.state.ActiveMachine != "kube2" {
		t.Fatalf("state = %#v", operator.state)
	}
	if got := machines.status.ProvisioningState; got != aksmachine.ProvisioningStateSucceeded {
		t.Fatalf("status = %s", got)
	}
}

func TestRepaveReconcilerResetDelete(t *testing.T) {
	t.Parallel()

	machines := &fakeMachineClient{notFound: true}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1"}, Spec: corev1.NodeSpec{Taints: []corev1.Taint{deletionTaint()}}}
	operator := &fakeWorkerRuntime{}
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
	repaves := newTestRepaveReconciler(t, machines, fakeClient(), &fakeWorkerRuntime{err: errors.New("bad state")})

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
	operator := &fakeWorkerRuntime{state: &State{AppliedSettingsVersion: "41", AppliedKubernetesVersion: "1.33.0", ActiveMachine: "kube1"}}
	repaves := newTestRepaveReconciler(t, machines, fakeClient(), operator)

	err := repaves.reconcileOnce(t.Context())
	if err == nil || !strings.Contains(err.Error(), "validate AKS machine snapshot") {
		t.Fatalf("Reconcile error = %v, want invalid machine snapshot", err)
	}
	if operator.applied {
		t.Fatal("ApplyGoalState was called for an invalid machine goal")
	}
}

func newTestRepaveReconciler(t *testing.T, machines aksmachine.MachineClient, kubeClient client.Client, operator *fakeWorkerRuntime) *repaveReconciler {
	t.Helper()
	repaves, err := newRepaveReconciler(repaveReconcilerOptions{
		Log:      slog.Default(),
		Machines: machines,
		Client:   kubeClient,
		Runtime:  operator,
		State:    operator,
		Config: &config.Config{
			Bootstrap: config.BootstrapConfig{
				Isolation: config.DefaultIsolationMode,
			},
		},
		Refresher: noopBootstrapDataRefresher{},
		HookUnits: func(*slog.Logger) phases.Task { return testTask{name: "hooks"} },
		Uninstall: operator.uninstall,
		ClearConfig: testTask{
			name: "remove-runtime-dirs",
			run: func() error {
				operator.state = nil
				return nil
			},
		},
		NodeName: "node1",
	})
	if err != nil {
		t.Fatalf("newRepaveReconciler: %v", err)
	}
	repaves.resolve = fakeResolveMachine
	return repaves
}

func fakeClient(objects ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(objects...).Build()
}

type fakeMachineClient struct {
	machine  *aksmachine.Machine
	status   aksmachine.Status
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
	return nil
}

type fakeWorkerRuntime struct {
	state        *State
	err          error
	restartErr   error
	resetErr     error
	stopErr      error
	applied      bool
	restarted    bool
	reset        bool
	stopped      bool
	restartInput agentruntime.Inputs
}

func (f *fakeWorkerRuntime) Load(context.Context) (*State, error) {
	return f.state, f.err
}

func (f *fakeWorkerRuntime) Save(_ context.Context, state *State) error {
	f.state = state
	return nil
}

func (f *fakeWorkerRuntime) Delete(context.Context) error {
	f.state = nil
	return nil
}

func (f *fakeWorkerRuntime) Worker(log *slog.Logger, node agentruntime.Node) (agentruntime.WorkerContext, error) {
	return (agentruntime.NSpawn{}).Worker(log, node)
}

func (f *fakeWorkerRuntime) Start(_ *slog.Logger, _ agentruntime.Inputs) phases.Task {
	return testTask{
		name: "start",
		run: func() error {
			f.applied = true
			return nil
		},
	}
}

func (f *fakeWorkerRuntime) Restart(_ *slog.Logger, input agentruntime.Inputs) phases.Task {
	f.restartInput = input
	return testTask{
		name: "restart",
		run: func() error {
			f.restarted = true
			return f.restartErr
		},
	}
}

func (f *fakeWorkerRuntime) PrepareUpgrade(_ *slog.Logger, _ agentruntime.Inputs) phases.Task {
	return testTask{name: "prepare-upgrade"}
}

func (f *fakeWorkerRuntime) Recover(_ *slog.Logger, _ agentruntime.Inputs) phases.Task {
	return testTask{name: "recover"}
}

func (f *fakeWorkerRuntime) Retire(_ *slog.Logger, _ agentruntime.Inputs) phases.Task {
	return testTask{name: "retire"}
}

func (f *fakeWorkerRuntime) PlanReset(_ *slog.Logger) (agentruntime.ResetPlan, error) {
	return testResetPlan{
		stop: testTask{name: "stop-reset-workers"},
		finish: testTask{
			name: "reset",
			run: func() error {
				f.reset = true
				return f.resetErr
			},
		},
	}, nil
}

func (f *fakeWorkerRuntime) uninstall(*slog.Logger) phases.Task {
	return testTask{
		name: "uninstall",
		run: func() error {
			f.stopped = true
			return f.stopErr
		},
	}
}

type testTask struct {
	name string
	run  func() error
}

func (t testTask) Name() string { return t.name }

func (t testTask) Do(context.Context) error {
	if t.run != nil {
		return t.run()
	}
	return nil
}

func fakeResolveMachine(context.Context, *slog.Logger, *config.Config, string) (config.ResolvedMachineGoal, error) {
	return config.ResolvedMachineGoal{GoalState: &goalstates.MachineGoalState{}}, nil
}
