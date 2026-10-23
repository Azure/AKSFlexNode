package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var errHubTest = errors.New("injected hub failure")

type testHubOperations struct {
	calls   []hubPhase
	fail    hubPhase
	failErr error // defaults to errHubTest
}

func (o *testHubOperations) step(phase hubPhase) error {
	o.calls = append(o.calls, phase)
	if o.fail == phase {
		if o.failErr != nil {
			return o.failErr
		}
		return errHubTest
	}
	return nil
}

func (o *testHubOperations) Prepare(context.Context) error  { return o.step(hubPrepare) }
func (o *testHubOperations) Route(context.Context) error    { return o.step(hubRoute) }
func (o *testHubOperations) Verify(context.Context) error   { return o.step(hubVerify) }
func (o *testHubOperations) Restore(context.Context) error  { return o.step(hubRestore) }
func (o *testHubOperations) Cleanup(context.Context) error  { return o.step(hubCleanup) }
func (o *testHubOperations) Stop(context.Context) error     { return o.step(hubStop) }
func (o *testHubOperations) Converge(context.Context) error { return o.step(hubConverge) }

// hubConverge labels Converge calls in testHubOperations; it is not a journal phase.
const hubConverge hubPhase = "converge"

type failingHubStore struct {
	hubTransitionStore
	saves  int
	failAt int
}

func (s *failingHubStore) Save(ctx context.Context, state *hubTransitionState) error {
	s.saves++
	if s.saves == s.failAt {
		return errHubTest
	}
	return s.hubTransitionStore.Save(ctx, state)
}

func hubTestTransition(t *testing.T, store hubTransitionStore, ops hubTransitionOperations) *hubTransition {
	t.Helper()
	r, err := newHubTransition(store, ops)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// newHubTestStore returns a journal. Tests exercising disable seed a completed
// enable first, because an initial false with no journal is a recorded no-op.
func newHubTestStore(t *testing.T, scope string, enabledBefore bool) fileHubTransitionStore {
	t.Helper()
	file := fileHubTransitionStore{root: t.TempDir(), path: "hub.json"}
	if enabledBefore {
		if err := file.Save(t.Context(), &hubTransitionState{Version: 2, Scope: scope, Accepted: ptr.To(true), Applied: ptr.To(true)}); err != nil {
			t.Fatal(err)
		}
	}
	return file
}

func TestHubTransitionInitialFalseIsNoop(t *testing.T) {
	t.Parallel()
	file := newHubTestStore(t, "scope", false)
	ops := &testHubOperations{}
	r := hubTestTransition(t, file, ops)
	for range 2 {
		if err := r.Reconcile(t.Context(), "scope", ptr.To(false)); err != nil {
			t.Fatal(err)
		}
	}
	if len(ops.calls) != 0 {
		t.Fatalf("never-enabled worker ran teardown: %v", ops.calls)
	}
	state, err := file.Load(t.Context())
	if err != nil || state == nil || state.Phase != "" || state.Applied == nil || *state.Applied {
		t.Fatalf("disabled state not recorded: %+v, %v", state, err)
	}
	if err := r.Reconcile(t.Context(), "scope", ptr.To(true)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ops.calls, []hubPhase{hubPrepare, hubRoute, hubVerify}) {
		t.Fatalf("later enable = %v", ops.calls)
	}
}

func TestHubTransitionCheckpointRecovery(t *testing.T) {
	t.Parallel()
	for _, enabled := range []bool{false, true} {
		name := "disable"
		steps := []hubPhase{hubRestore, hubCleanup, hubStop}
		if enabled {
			name = "enable"
			steps = []hubPhase{hubPrepare, hubRoute, hubVerify}
		}
		for _, checkpoint := range []int{1, 2, 3, 4} {
			t.Run(name+"/"+strconv.Itoa(checkpoint), func(t *testing.T) {
				t.Parallel()
				file := newHubTestStore(t, "generation-1", !enabled)
				store := &failingHubStore{hubTransitionStore: file, failAt: checkpoint}
				ops := &testHubOperations{}
				intent := ptr.To(enabled)
				if err := hubTestTransition(t, store, ops).Reconcile(t.Context(), "generation-1", intent); !errors.Is(err, errHubTest) {
					t.Fatalf("checkpoint error = %v", err)
				}
				if len(ops.calls) != checkpoint-1 {
					t.Fatalf("operations before failed checkpoint = %v", ops.calls)
				}
				state, err := file.Load(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if state != nil && state.Phase == "" && state.Applied != nil && *state.Applied == *intent {
					t.Fatal("acknowledged an incomplete transition")
				}
				// A new engine and store instance simulate a daemon restart. The
				// operation whose checkpoint failed must be safe to repeat.
				ops.calls = nil
				r := hubTestTransition(t, file, ops)
				if err := r.Reconcile(t.Context(), "generation-1", intent); err != nil {
					t.Fatal(err)
				}
				start := max(0, checkpoint-2)
				if !reflect.DeepEqual(ops.calls, steps[start:]) {
					t.Fatalf("recovery operations = %v, want %v", ops.calls, steps[start:])
				}
				ops.calls = nil
				var wantRepeat []hubPhase
				if enabled {
					wantRepeat = []hubPhase{hubConverge} // steady enabled Hub converges only
				}
				if err := r.Reconcile(t.Context(), "generation-1", intent); err != nil || !reflect.DeepEqual(ops.calls, wantRepeat) {
					t.Fatalf("repeated applied intent = %v, %v", ops.calls, err)
				}
				state, err = file.Load(t.Context())
				if err != nil || state.Applied == nil || *state.Applied != *intent || state.Phase != "" {
					t.Fatalf("completed state = %+v, %v", state, err)
				}
			})
		}
	}
}

func TestHubTransitionRepeatedBoolWithoutPolicyChange(t *testing.T) {
	t.Parallel()
	file := fileHubTransitionStore{root: t.TempDir(), path: "hub.json"}
	ops := &testHubOperations{}
	r := hubTestTransition(t, file, ops)
	if err := r.Reconcile(t.Context(), "scope", ptr.To(true)); err != nil {
		t.Fatal(err)
	}
	ops.calls = nil
	intent := ptr.To(true)
	if err := r.Reconcile(t.Context(), "scope", intent); err != nil || !reflect.DeepEqual(ops.calls, []hubPhase{hubConverge}) {
		t.Fatalf("unchanged enablement repeated initialization: %v, %v", ops.calls, err)
	}
	state, err := file.Load(t.Context())
	if err != nil || state.Applied == nil || *state.Applied != *intent {
		t.Fatalf("applied intent = %+v, %v", state, err)
	}
	info, err := os.Stat(filepath.Join(file.root, file.path))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("journal permissions = %v, %v", info, err)
	}
}

func TestHubTransitionConvergesOnlyWhenSteadyEnabled(t *testing.T) {
	t.Parallel()
	file := newHubTestStore(t, "scope", false)
	ops := &testHubOperations{}
	r := hubTestTransition(t, file, ops)
	steps := []struct {
		intent bool
		want   []hubPhase
	}{
		{intent: false}, // never enabled: no-op, no converge
		{intent: true, want: []hubPhase{hubPrepare, hubRoute, hubVerify}}, // enable: no converge in the same read
		{intent: true, want: []hubPhase{hubConverge}},                     // steady enabled: converge
		{intent: true, want: []hubPhase{hubConverge}},
		{intent: false, want: []hubPhase{hubRestore, hubCleanup, hubStop}}, // disable: no converge
		{intent: false},
	}
	for i, step := range steps {
		ops.calls = nil
		if err := r.Reconcile(t.Context(), "scope", ptr.To(step.intent)); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if !reflect.DeepEqual(ops.calls, step.want) {
			t.Fatalf("step %d calls = %v, want %v", i, ops.calls, step.want)
		}
	}
	// A converge failure is returned for retry and leaves the journal applied.
	ops.calls, ops.fail = nil, ""
	if err := r.Reconcile(t.Context(), "scope", ptr.To(true)); err != nil {
		t.Fatal(err)
	}
	ops.fail = hubConverge
	if err := r.Reconcile(t.Context(), "scope", ptr.To(true)); !errors.Is(err, errHubTest) {
		t.Fatalf("converge error = %v", err)
	}
	state, err := file.Load(t.Context())
	if err != nil || state.Phase != "" || state.Applied == nil || !*state.Applied {
		t.Fatalf("converge failure changed the journal: %+v, %v", state, err)
	}
}

func TestHubTransitionOptionalBool(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"omitted", "null", "false", "true", "wrong type"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// This fixture verifies bool presence semantics, not an RP field name.
			payload := `{}`
			if name != "omitted" {
				value := name
				if name == "wrong type" {
					value = `"false"`
				}
				payload = `{"enabled":` + value + `}`
			}
			var input struct {
				Enabled *bool `json:"enabled"`
			}
			err := json.Unmarshal([]byte(payload), &input)
			if name == "wrong type" {
				if err == nil {
					t.Fatal("string bool accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			file := fileHubTransitionStore{root: t.TempDir(), path: "hub.json"}
			ops := &testHubOperations{}
			if err := hubTestTransition(t, file, ops).Reconcile(t.Context(), "scope", input.Enabled); err != nil {
				t.Fatal(err)
			}
			state, err := file.Load(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if input.Enabled == nil {
				if state != nil || len(ops.calls) != 0 {
					t.Fatal("absent bool changed state")
				}
			} else if state == nil || state.Applied == nil || *state.Applied != *input.Enabled {
				t.Fatalf("explicit bool was not applied: %+v", state)
			}
		})
	}
}

func TestHubTransitionDisableAfterInterruptedReenable(t *testing.T) {
	t.Parallel()
	file := fileHubTransitionStore{root: t.TempDir(), path: "hub.json"}
	ops := &testHubOperations{}
	r := hubTestTransition(t, file, ops)
	if err := r.Reconcile(t.Context(), "scope", ptr.To(false)); err != nil {
		t.Fatal(err)
	}
	ops.fail = hubVerify
	if err := r.Reconcile(t.Context(), "scope", ptr.To(true)); !errors.Is(err, errHubTest) {
		t.Fatal(err)
	}
	state, err := file.Load(t.Context())
	if err != nil || state.Applied == nil || *state.Applied || state.Phase != hubVerify {
		t.Fatalf("last completed disable lost: %+v, %v", state, err)
	}
	ops.fail, ops.calls = "", nil
	if err := r.Reconcile(t.Context(), "scope", ptr.To(false)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ops.calls, []hubPhase{hubRestore, hubCleanup, hubStop}) {
		t.Fatalf("partial routing change not restored: %v", ops.calls)
	}
}

func TestHubTransitionOperationFailures(t *testing.T) {
	t.Parallel()
	for _, phase := range []hubPhase{hubPrepare, hubRoute, hubVerify, hubRestore, hubCleanup, hubStop} {
		t.Run(string(phase), func(t *testing.T) {
			t.Parallel()
			intent := ptr.To(phase == hubPrepare || phase == hubRoute || phase == hubVerify)
			file := newHubTestStore(t, "scope", !*intent)
			ops := &testHubOperations{fail: phase}
			r := hubTestTransition(t, file, ops)
			if err := r.Reconcile(t.Context(), "scope", intent); !errors.Is(err, errHubTest) {
				t.Fatalf("operation error = %v", err)
			}
			state, err := file.Load(t.Context())
			if err != nil || state.Phase != phase || (state.Applied != nil && *state.Applied == *intent) {
				t.Fatalf("failed phase state = %+v, %v", state, err)
			}
			if ops.calls[len(ops.calls)-1] != phase {
				t.Fatalf("operations continued after failure: %v", ops.calls)
			}
			ops.fail, ops.calls = "", nil
			if err := r.Reconcile(t.Context(), "scope", intent); err != nil {
				t.Fatal(err)
			}
			if ops.calls[0] != phase {
				t.Fatalf("retry started at %s, want %s", ops.calls[0], phase)
			}
		})
	}
}

func TestHubTransitionIntentChanges(t *testing.T) {
	t.Parallel()
	for _, phase := range []hubPhase{hubPrepare, hubRoute, hubVerify, hubRestore, hubCleanup, hubStop} {
		t.Run(string(phase), func(t *testing.T) {
			t.Parallel()
			enabled := phase == hubPrepare || phase == hubRoute || phase == hubVerify
			file := newHubTestStore(t, "scope", !enabled)
			ops := &testHubOperations{fail: phase}
			r := hubTestTransition(t, file, ops)
			if err := r.Reconcile(t.Context(), "scope", ptr.To(enabled)); !errors.Is(err, errHubTest) {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(file.root, file.path))
			if err != nil {
				t.Fatal(err)
			}
			ops.fail, ops.calls = "", nil
			// Omission/loss of authority must not resume stale accepted intent.
			if err := r.Reconcile(t.Context(), "scope", nil); err != nil || len(ops.calls) != 0 {
				t.Fatalf("absent intent = %v, %v", ops.calls, err)
			}
			after, err := os.ReadFile(filepath.Join(file.root, file.path))
			if err != nil || string(before) != string(after) {
				t.Fatal("absent intent modified the journal")
			}
			if err := r.Reconcile(t.Context(), "scope", ptr.To(!enabled)); err != nil {
				t.Fatal(err)
			}
			want := []hubPhase{hubPrepare, hubRoute, hubVerify}
			if enabled {
				want = []hubPhase{hubRestore, hubCleanup, hubStop}
			}
			if !reflect.DeepEqual(ops.calls, want) {
				t.Fatalf("superseding intent = %v, want %v", ops.calls, want)
			}
		})
	}
}

func TestHubTransitionRejectsAmbiguousState(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"replacement", "missing accepted", "old journal", "corrupt", "future version", "invalid phase", "missing applied", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			file := fileHubTransitionStore{root: t.TempDir(), path: "hub.json"}
			ops := &testHubOperations{}
			r := hubTestTransition(t, file, ops)
			intent := ptr.To(true)
			if err := r.Reconcile(t.Context(), "scope", intent); err != nil {
				t.Fatal(err)
			}
			ops.calls = nil
			scope, ctx := "scope", t.Context()
			switch name {
			case "replacement":
				scope = "new-generation"
			case "missing accepted":
				writeHubJournal(t, filepath.Join(file.root, file.path), `{"version":2,"scope":"scope","phase":"restore"}`)
			case "old journal":
				writeHubJournal(t, filepath.Join(file.root, file.path), `{"version":1,"scope":"scope","accepted":{"revision":"1","enabled":true},"phase":"route"}`)
			case "corrupt":
				writeHubJournal(t, filepath.Join(file.root, file.path), "{")
			case "future version":
				writeHubJournal(t, filepath.Join(file.root, file.path), `{"version":3,"scope":"scope","accepted":false,"phase":"restore"}`)
			case "invalid phase":
				writeHubJournal(t, filepath.Join(file.root, file.path), `{"version":2,"scope":"scope","accepted":false,"phase":"route"}`)
			case "missing applied":
				writeHubJournal(t, filepath.Join(file.root, file.path), `{"version":2,"scope":"scope","accepted":true}`)
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if err := r.Reconcile(ctx, scope, intent); err == nil || len(ops.calls) != 0 {
				t.Fatalf("ambiguous transition executed: %v, %v", ops.calls, err)
			}
		})
	}
}

func writeHubJournal(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

type testHubMachineReconciler struct {
	calls int
	err   error
}

func (r *testHubMachineReconciler) ReconcileHub(context.Context, aksmachine.GoalState, *State, *corev1.Node) error {
	r.calls++
	return r.err
}

func TestHubUsesExistingMachineQueue(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"applied machine", "pending repave", "hub retry", "node absent", "deleting", "machine absent"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			machines := &fakeMachineClient{machine: &aksmachine.Machine{Goal: aksmachine.GoalState{SettingsVersion: "42", KubernetesVersion: "1.34.0"}}}
			operator := &fakeNodeOperator{state: &State{AppliedSettingsVersion: "42", ActiveMachine: "kube1"}}
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

func TestHubTransitionStoreConfinedToWorker(t *testing.T) {
	t.Parallel()
	worker, outside := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(worker, "var/lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(worker, "var/lib/aks-flex-node")); err != nil {
		t.Fatal(err)
	}
	store := fileHubTransitionStore{root: worker, path: hubWorkerJournal}
	state := &hubTransitionState{Version: 2, Scope: "kube1", Accepted: ptr.To(false), Applied: ptr.To(false)}
	if err := store.Save(t.Context(), state); err == nil {
		t.Fatal("journal write escaped worker rootfs")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("wrote outside worker: %v", entries)
	}
}
