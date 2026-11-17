package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/internal/agentruntime"
	"github.com/Azure/AKSFlexNode/pkg/yurthub"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var errHubTest = errors.New("injected hub failure")

type testHubMachineReconciler struct {
	calls  int
	err    error
	intent *bool
	worker string
	root   string
	actual yurthub.Worker
	node   *corev1.Node
}

func (r *testHubMachineReconciler) Reconcile(_ context.Context, intent *bool, worker yurthub.Worker, node *corev1.Node) error {
	r.calls++
	r.intent, r.worker, r.node = intent, worker.Scope, node
	r.actual = worker
	if worker.Root != nil {
		r.root = worker.Root.Path("/")
	}
	return r.err
}

func TestHubTranslation(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		state  *State
		intent *bool
		worker string
		root   string
	}{
		"first worker": {
			state:  &State{ActiveMachine: "kube1"},
			intent: ptr.To(true),
			worker: "kube1",
			root:   "/var/lib/machines/kube1",
		},
		"second worker": {
			state:  &State{ActiveMachine: "kube2"},
			intent: ptr.To(false),
			worker: "kube2",
			root:   "/var/lib/machines/kube2",
		},
		"nil intent": {
			state:  &State{ActiveMachine: "kube1"},
			worker: "kube1",
			root:   "/var/lib/machines/kube1",
		},
		"no state": {intent: ptr.To(true)},
		"invalid worker": {
			state:  &State{ActiveMachine: "kube3"},
			intent: ptr.To(true),
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			hub := &testHubMachineReconciler{}
			r := &repaveReconciler{hub: hub, runtime: agentruntime.NSpawn{}}
			node := &corev1.Node{}
			machine := &aksmachine.Machine{Goal: aksmachine.GoalState{DisconnectToleranceEnabled: tc.intent}}
			r.reconcileHub(t.Context(), machineSnapshot{machine: machine}, nodeSnapshot{node: node}, tc.state)
			if hub.calls != 1 || hub.intent != tc.intent || hub.worker != tc.worker || hub.root != tc.root || hub.node != node {
				t.Fatalf("hub got calls=%d intent=%v worker=%q root=%q node=%p", hub.calls, hub.intent, hub.worker, hub.root, hub.node)
			}
			if tc.worker != "" && (hub.actual.Root.Slot() != tc.worker || hub.actual.Exec == nil || hub.actual.KubeletUnit != "kubelet.service" || hub.actual.Mode != isolation.ModeNSpawn) {
				t.Fatalf("unexpected worker context: %+v", hub.actual)
			}
		})
	}
}

func TestHubReadsIntentWithoutRepave(t *testing.T) {
	t.Parallel()
	machines := &fakeMachineClient{
		machine: &aksmachine.Machine{
			Goal: aksmachine.GoalState{
				SettingsVersion:   "42",
				KubernetesVersion: "1.34.0",
			},
		},
	}
	operator := &fakeWorkerRuntime{
		state: &State{
			AppliedSettingsVersion:   "42",
			AppliedKubernetesVersion: "1.34.0",
			ActiveMachine:            "kube1",
		},
	}
	r := newTestRepaveReconciler(t, machines, fakeClient(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1"}}), operator)
	hub := &testHubMachineReconciler{}
	r.hub = hub

	for i, intent := range []*bool{ptr.To(false), ptr.To(true), ptr.To(true), nil, ptr.To(false), ptr.To(true)} {
		machines.machine.Goal.DisconnectToleranceEnabled = intent
		if err := r.reconcileOnce(t.Context()); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if hub.calls != i+1 || hub.intent != intent || hub.worker != "kube1" || hub.root != "/var/lib/machines/kube1" || hub.node == nil {
			t.Fatalf("step %d: hub did not receive current intent, worker and Node: %+v", i, hub)
		}
		if operator.applied || operator.state.AppliedSettingsVersion != "42" {
			t.Fatalf("step %d: hub change triggered repave or changed Machine settings", i)
		}
	}
}

func TestHubUsesExistingMachineQueue(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"applied machine", "pending repave", "hub retry", "node absent", "deleting", "machine absent"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			machines := &fakeMachineClient{
				machine: &aksmachine.Machine{
					Goal: aksmachine.GoalState{
						SettingsVersion:   "42",
						KubernetesVersion: "1.34.0",
					},
				},
			}
			operator := &fakeWorkerRuntime{state: &State{AppliedSettingsVersion: "42", ActiveMachine: "kube1"}}
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1"}}
			hub := &testHubMachineReconciler{}
			wantCalls := 1
			switch name {
			case "pending repave":
				operator.state.AppliedSettingsVersion = "41"
			case "hub retry":
				hub.err = errHubTest
			case "node absent", "deleting", "machine absent":
				wantCalls = 0
			}
			if name == "deleting" {
				node.Spec.Taints = []corev1.Taint{deletionTaint()}
			}
			if name == "machine absent" {
				machines.notFound = true
			}
			var objects []client.Object
			if name != "node absent" {
				objects = append(objects, node)
			}
			r := newTestRepaveReconciler(t, machines, fakeClient(objects...), operator)
			r.hub = hub
			before := *operator.state
			result, err := r.ReconcileRepave(t.Context(), repaveByAKSMachine)
			// Hub failures are logged, never returned: the normal jittered poll
			// must stay in effect instead of the controller's error backoff.
			if err != nil || hub.calls != wantCalls {
				t.Fatalf("hub queue calls = %d, error = %v", hub.calls, err)
			}
			if result.RequeueAfter < r.machineReconcileInterval {
				t.Fatalf("RequeueAfter = %v, want the machine poll interval", result.RequeueAfter)
			}
			if *operator.state != before || operator.applied || operator.reset || operator.restarted {
				t.Fatal("hub reconcile altered node lifecycle or acknowledged pending settings")
			}
			if name == "pending repave" && machines.status.ObservedSettingsVersion != "" {
				t.Fatal("hub reconcile acknowledged Machine settings")
			}
			// A Hub failure must not skip the existing decision: already-applied
			// Machine settings are still reported.
			if name == "hub retry" && machines.status.ObservedSettingsVersion != "42" {
				t.Fatal("hub failure skipped Machine status reporting")
			}
		})
	}
}

type orderRecordingHub struct {
	machines  *fakeMachineClient
	sawStatus aksmachine.ProvisioningState
	calls     int
}

func (h *orderRecordingHub) Reconcile(context.Context, *bool, yurthub.Worker, *corev1.Node) error {
	h.calls++
	h.sawStatus = h.machines.status.ProvisioningState
	return errHubTest
}

// Hub work can download and wait for readiness, so Machine status must be
// reported first, and a Hub failure must not fail the reconcile.
func TestReconcilerReportsMachineStatusBeforeHub(t *testing.T) {
	t.Parallel()
	machines := &fakeMachineClient{
		machine: &aksmachine.Machine{
			Goal: aksmachine.GoalState{
				SettingsVersion:            "42",
				KubernetesVersion:          "1.34.0",
				DisconnectToleranceEnabled: ptr.To(true),
			},
		},
	}
	operator := &fakeWorkerRuntime{
		state: &State{
			AppliedSettingsVersion:   "42",
			AppliedKubernetesVersion: "1.34.0",
			ActiveMachine:            "kube1",
		},
	}
	r := newTestRepaveReconciler(t, machines, fakeClient(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1"}}), operator)
	hub := &orderRecordingHub{machines: machines}
	r.hub = hub
	if err := r.reconcileOnce(t.Context()); err != nil {
		t.Fatalf("hub failure failed the reconcile: %v", err)
	}
	if hub.calls != 1 || hub.sawStatus != aksmachine.ProvisioningStateSucceeded {
		t.Fatalf("hub ran %d times, saw Machine status %q; want status reported first", hub.calls, hub.sawStatus)
	}
}
