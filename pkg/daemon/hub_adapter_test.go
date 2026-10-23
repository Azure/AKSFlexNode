package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/yurthub"
)

func newTestHubAdapter(t *testing.T, ops *testHubOperations) (*hubMachineAdapter, string) {
	t.Helper()
	machinesDir := t.TempDir()
	for _, machine := range []string{"kube1", "kube2"} {
		if err := os.MkdirAll(filepath.Join(machinesDir, machine), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	adapter, err := newHubMachineAdapter(func(string) (hubTransitionOperations, error) { return ops, nil })
	if err != nil {
		t.Fatal(err)
	}
	adapter.machinesDir = machinesDir
	return adapter, machinesDir
}

func TestHubAdapterFollowsIntentWithUnchangedETag(t *testing.T) {
	t.Parallel()
	ops := &testHubOperations{}
	adapter, machinesDir := newTestHubAdapter(t, ops)
	machines := &fakeMachineClient{machine: &aksmachine.Machine{Goal: aksmachine.GoalState{SettingsVersion: "42", KubernetesVersion: "1.34.0"}}}
	operator := &fakeNodeOperator{state: &State{AppliedSettingsVersion: "42", AppliedKubernetesVersion: "1.34.0", ActiveMachine: "kube1"}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1"}}
	r := newTestRepaveReconciler(t, machines, fakeClient(node), operator)
	r.hub = adapter

	steps := []struct {
		intent *bool
		want   []hubPhase
	}{
		{intent: ptr.To(false)}, // RP toggle off before rollout: no-op
		{intent: ptr.To(true), want: []hubPhase{hubPrepare, hubRoute, hubVerify}},
		{intent: ptr.To(true), want: []hubPhase{hubConverge}}, // repeated read: converge only, no reinitialization
		{intent: nil}, // undecodable value: keep state
		{intent: ptr.To(false), want: []hubPhase{hubRestore, hubCleanup, hubStop}},
		{intent: ptr.To(true), want: []hubPhase{hubPrepare, hubRoute, hubVerify}},
	}
	for i, step := range steps {
		ops.calls = nil
		machines.machine.Goal.DisconnectToleranceEnabled = step.intent
		if err := r.reconcileOnce(t.Context()); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if !reflect.DeepEqual(ops.calls, step.want) {
			t.Fatalf("step %d operations = %v, want %v", i, ops.calls, step.want)
		}
		if operator.applied || operator.state.AppliedSettingsVersion != "42" {
			t.Fatalf("step %d: hub change triggered repave or changed Machine settings", i)
		}
	}
	if _, err := os.Stat(filepath.Join(machinesDir, "kube1", hubWorkerJournal)); err != nil {
		t.Fatalf("journal not stored in worker rootfs: %v", err)
	}
}

func TestHubAdapterRepavedWorkerStartsFresh(t *testing.T) {
	t.Parallel()
	ops := &testHubOperations{}
	adapter, machinesDir := newTestHubAdapter(t, ops)
	goal := aksmachine.GoalState{DisconnectToleranceEnabled: ptr.To(true)}
	if err := adapter.ReconcileHub(t.Context(), goal, &State{ActiveMachine: "kube1"}, nil); err != nil {
		t.Fatal(err)
	}
	ops.calls = nil
	// A repaved worker has its own rootfs and no Hub yet, so enablement reruns.
	if err := adapter.ReconcileHub(t.Context(), goal, &State{ActiveMachine: "kube2"}, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ops.calls, []hubPhase{hubPrepare, hubRoute, hubVerify}) {
		t.Fatalf("repaved worker operations = %v", ops.calls)
	}
	if _, err := os.Stat(filepath.Join(machinesDir, "kube2", hubWorkerJournal)); err != nil {
		t.Fatalf("new worker journal missing: %v", err)
	}
}

func TestHubAdapterUnsupportedWorkerIsNotRetried(t *testing.T) {
	t.Parallel()
	ops := &testHubOperations{fail: hubPrepare, failErr: fmt.Errorf("prepare: %w", yurthub.ErrUnsupportedWorker)}
	adapter, _ := newTestHubAdapter(t, ops)
	clock := time.Unix(0, 0)
	adapter.now = func() time.Time { return clock }
	state := &State{ActiveMachine: "kube1"}
	for range 2 {
		if err := adapter.ReconcileHub(t.Context(), aksmachine.GoalState{DisconnectToleranceEnabled: ptr.To(true)}, state, nil); err != nil {
			t.Fatalf("unsupported worker returned an error for retry: %v", err)
		}
	}
	// The second read is inside the retry window, so only one attempt.
	if !reflect.DeepEqual(ops.calls, []hubPhase{hubPrepare}) {
		t.Fatalf("operations = %v, want one prepare attempt", ops.calls)
	}
	clock = clock.Add(hubRetryMax)
	if err := adapter.ReconcileHub(t.Context(), aksmachine.GoalState{DisconnectToleranceEnabled: ptr.To(true)}, state, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ops.calls, []hubPhase{hubPrepare, hubPrepare}) {
		t.Fatalf("operations = %v, want a retry after the window", ops.calls)
	}
	// Turning DT off still converges through the disable steps.
	ops.calls, ops.fail = nil, ""
	if err := adapter.ReconcileHub(t.Context(), aksmachine.GoalState{DisconnectToleranceEnabled: ptr.To(false)}, state, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ops.calls, []hubPhase{hubRestore, hubCleanup, hubStop}) {
		t.Fatalf("disable after unsupported = %v", ops.calls)
	}
}

func TestHubAdapterBacksOffFailures(t *testing.T) {
	t.Parallel()
	ops := &testHubOperations{fail: hubPrepare}
	adapter, _ := newTestHubAdapter(t, ops)
	clock := time.Unix(0, 0)
	adapter.now = func() time.Time { return clock }
	state := &State{ActiveMachine: "kube1"}
	enable := aksmachine.GoalState{DisconnectToleranceEnabled: ptr.To(true)}
	attempts := func() int { return len(ops.calls) }

	if err := adapter.ReconcileHub(t.Context(), enable, state, nil); !errors.Is(err, errHubTest) {
		t.Fatalf("first failure = %v", err)
	}
	// Node events inside the window don't retry.
	if err := adapter.ReconcileHub(t.Context(), enable, state, nil); err != nil || attempts() != 1 {
		t.Fatalf("retried inside the window: %v, attempts=%d", err, attempts())
	}
	// The delay doubles: retry after 1m, then the next is not due at +1m.
	clock = clock.Add(hubRetryMin)
	if err := adapter.ReconcileHub(t.Context(), enable, state, nil); !errors.Is(err, errHubTest) || attempts() != 2 {
		t.Fatalf("no retry after 1m: %v, attempts=%d", err, attempts())
	}
	clock = clock.Add(hubRetryMin)
	if err := adapter.ReconcileHub(t.Context(), enable, state, nil); err != nil || attempts() != 2 {
		t.Fatalf("delay did not double: %v, attempts=%d", err, attempts())
	}
	// A changed intent is acted on immediately.
	ops.fail = ""
	if err := adapter.ReconcileHub(t.Context(), aksmachine.GoalState{DisconnectToleranceEnabled: ptr.To(false)}, state, nil); err != nil || attempts() != 5 {
		t.Fatalf("changed intent waited for the retry window: %v, calls=%v", err, ops.calls)
	}
	// Success clears the retry state.
	if err := adapter.ReconcileHub(t.Context(), enable, state, nil); err != nil || attempts() != 8 {
		t.Fatalf("enable after success did not run: %v, calls=%v", err, ops.calls)
	}
}

func TestHubAdapterFalseWithoutWorkerIsNoop(t *testing.T) {
	t.Parallel()
	ops := &testHubOperations{}
	adapter, _ := newTestHubAdapter(t, ops)
	if err := adapter.ReconcileHub(t.Context(), aksmachine.GoalState{DisconnectToleranceEnabled: ptr.To(false)}, nil, nil); err != nil || len(ops.calls) != 0 {
		t.Fatalf("false without an active worker = %v, %v", ops.calls, err)
	}
}

func TestHubAdapterRejectsInvalidState(t *testing.T) {
	t.Parallel()
	for name, state := range map[string]*State{"missing": nil, "invalid machine": {ActiveMachine: "kube3"}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ops := &testHubOperations{}
			adapter, _ := newTestHubAdapter(t, ops)
			err := adapter.ReconcileHub(t.Context(), aksmachine.GoalState{DisconnectToleranceEnabled: ptr.To(true)}, state, nil)
			if err == nil || len(ops.calls) != 0 {
				t.Fatalf("invalid state ran operations: %v, %v", ops.calls, err)
			}
		})
	}
	if _, err := newHubMachineAdapter(nil); err == nil {
		t.Fatal("nil operations factory accepted")
	}
}

// liveNode returns a Node whose status reflects condition writes, like the API.
func liveNode(adapter *hubMachineAdapter) (*corev1.Node, *[]corev1.NodeCondition) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1"}}
	var written []corev1.NodeCondition
	adapter.setCondition = func(_ context.Context, c corev1.NodeCondition) error {
		written = append(written, c)
		node.Status.Conditions = []corev1.NodeCondition{c}
		return nil
	}
	return node, &written
}

func TestHubAdapterReportsNodeCondition(t *testing.T) {
	t.Parallel()
	ops := &testHubOperations{}
	adapter, _ := newTestHubAdapter(t, ops)
	clock := time.Unix(0, 0)
	adapter.now = func() time.Time { return clock }
	node, writes := liveNode(adapter)
	state := &State{ActiveMachine: "kube1"}
	reconcile := func(intent bool) {
		t.Helper()
		_ = adapter.ReconcileHub(t.Context(), aksmachine.GoalState{DisconnectToleranceEnabled: ptr.To(intent)}, state, node)
	}
	steps := []struct {
		intent     bool
		fail       error
		wantStatus corev1.ConditionStatus
		wantReason string
	}{
		{intent: true, fail: fmt.Errorf("prepare: %w", yurthub.ErrPackageDownload), wantStatus: corev1.ConditionFalse, wantReason: "PackageDownloadFailed"},
		{intent: true, fail: fmt.Errorf("prepare: %w", yurthub.ErrUnsupportedWorker), wantStatus: corev1.ConditionFalse, wantReason: "Unsupported"},
		{intent: true, fail: errHubTest, wantStatus: corev1.ConditionUnknown, wantReason: "EnableFailed"},
		{intent: true, wantStatus: corev1.ConditionTrue, wantReason: "Enabled"},
		{intent: false, wantStatus: corev1.ConditionFalse, wantReason: "Disabled"},
	}
	for i, step := range steps {
		ops.fail, ops.failErr = "", step.fail
		if step.fail != nil {
			ops.fail = hubPrepare
		}
		clock = clock.Add(hubRetryMax) // past any retry delay
		reconcile(step.intent)
		written := *writes
		if len(written) != i+1 {
			t.Fatalf("step %d: %d condition writes, want %d", i, len(written), i+1)
		}
		got := written[i]
		if got.Type != hubConditionType || got.Status != step.wantStatus || got.Reason != step.wantReason || got.Message == "" {
			t.Fatalf("step %d: condition = %+v", i, got)
		}
	}
	// Same result within the heartbeat interval: nothing to write.
	clock = clock.Add(time.Minute)
	reconcile(false)
	written := *writes
	if len(written) != len(steps) {
		t.Fatalf("unchanged condition written again: %d writes", len(written))
	}
	// Unchanged status keeps its transition time.
	if !written[1].LastTransitionTime.Equal(&written[0].LastTransitionTime) {
		t.Fatal("transition time moved without a status change")
	}
	// After the heartbeat interval the same condition is refreshed, keeping its transition time.
	clock = clock.Add(adapter.heartbeat)
	reconcile(false)
	written = *writes
	last, prev := written[len(written)-1], written[len(written)-2]
	if len(written) != len(steps)+1 || !last.LastHeartbeatTime.After(prev.LastHeartbeatTime.Time) || !last.LastTransitionTime.Equal(&prev.LastTransitionTime) {
		t.Fatalf("heartbeat not refreshed correctly: %+v", written)
	}
}

// The live Node is the source of truth: a removed or edited condition is
// restored, and an agent restart keeps the existing transition time.
func TestHubAdapterConditionFollowsLiveNode(t *testing.T) {
	t.Parallel()
	adapter, _ := newTestHubAdapter(t, &testHubOperations{})
	clock := time.Unix(1000, 0)
	adapter.now = func() time.Time { return clock }
	node, writes := liveNode(adapter)
	enable := aksmachine.GoalState{DisconnectToleranceEnabled: ptr.To(true)}
	state := &State{ActiveMachine: "kube1"}
	_ = adapter.ReconcileHub(t.Context(), enable, state, node)
	// Someone deletes the condition.
	node.Status.Conditions = nil
	clock = clock.Add(time.Minute)
	_ = adapter.ReconcileHub(t.Context(), enable, state, node)
	if len(*writes) != 2 {
		t.Fatalf("removed condition not restored: %+v", *writes)
	}
	// A restarted agent (fresh adapter) sees an unchanged, fresh condition: no write.
	restarted, _ := newTestHubAdapter(t, &testHubOperations{})
	restarted.now = adapter.now
	restartedWrites := 0
	restarted.setCondition = func(context.Context, corev1.NodeCondition) error { restartedWrites++; return nil }
	_ = restarted.ReconcileHub(t.Context(), enable, state, node)
	if restartedWrites != 0 {
		t.Fatal("restart rewrote an unchanged condition")
	}
}

func TestHubAdapterConditionWriteFailureDoesNotFailReconcile(t *testing.T) {
	t.Parallel()
	adapter, _ := newTestHubAdapter(t, &testHubOperations{})
	writes := 0
	adapter.setCondition = func(context.Context, corev1.NodeCondition) error { writes++; return errors.New("forbidden") }
	goal := aksmachine.GoalState{DisconnectToleranceEnabled: ptr.To(true)}
	for range 2 {
		if err := adapter.ReconcileHub(t.Context(), goal, &State{ActiveMachine: "kube1"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if writes != 2 {
		t.Fatalf("writes = %d, want a retry after a failed write", writes)
	}
}

func TestNodeConditionWriterKeepsOtherConditions(t *testing.T) {
	t.Parallel()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node1"},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
	client := fake.NewClientset(node)
	write := nodeConditionWriter(client.CoreV1().Nodes(), "node1")
	for _, status := range []corev1.ConditionStatus{corev1.ConditionFalse, corev1.ConditionTrue} {
		if err := write(t.Context(), corev1.NodeCondition{Type: hubConditionType, Status: status, Reason: "Test"}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := client.CoreV1().Nodes().Get(t.Context(), "node1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[corev1.NodeConditionType]corev1.ConditionStatus{}
	for _, c := range got.Status.Conditions {
		statuses[c.Type] = c.Status
	}
	if len(got.Status.Conditions) != 2 || statuses[corev1.NodeReady] != corev1.ConditionTrue || statuses[hubConditionType] != corev1.ConditionTrue {
		t.Fatalf("conditions = %+v", got.Status.Conditions)
	}
}

type orderRecordingHub struct {
	machines  *fakeMachineClient
	sawStatus aksmachine.ProvisioningState
	calls     int
}

func (h *orderRecordingHub) ReconcileHub(context.Context, aksmachine.GoalState, *State, *corev1.Node) error {
	h.calls++
	h.sawStatus = h.machines.status.ProvisioningState
	return errHubTest
}

// Hub work can download and wait for readiness, so Machine status must be
// reported first, and a Hub failure must not fail the reconcile.
func TestReconcilerReportsMachineStatusBeforeHub(t *testing.T) {
	t.Parallel()
	machines := &fakeMachineClient{machine: &aksmachine.Machine{Goal: aksmachine.GoalState{SettingsVersion: "42", KubernetesVersion: "1.34.0", DisconnectToleranceEnabled: ptr.To(true)}}}
	operator := &fakeNodeOperator{state: &State{AppliedSettingsVersion: "42", AppliedKubernetesVersion: "1.34.0", ActiveMachine: "kube1"}}
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

func TestHubAdapterConditionNewNodeAndRetriedWrites(t *testing.T) {
	t.Parallel()
	ops := &testHubOperations{}
	adapter, _ := newTestHubAdapter(t, ops)
	clock := time.Unix(0, 0)
	adapter.now = func() time.Time { return clock }
	var written []corev1.NodeCondition
	failWrites := false
	adapter.setCondition = func(_ context.Context, c corev1.NodeCondition) error {
		if failWrites {
			return errors.New("api unavailable")
		}
		written = append(written, c)
		return nil
	}
	enable := aksmachine.GoalState{DisconnectToleranceEnabled: ptr.To(true)}
	if err := adapter.ReconcileHub(t.Context(), enable, &State{ActiveMachine: "kube1"}, nil); err != nil {
		t.Fatal(err)
	}
	// A repaved worker registers a new Node object without the condition.
	if err := adapter.ReconcileHub(t.Context(), enable, &State{ActiveMachine: "kube2"}, &corev1.Node{}); err != nil {
		t.Fatal(err)
	}
	if len(written) != 2 || written[1].Reason != "Enabled" {
		t.Fatalf("new worker condition not written: %+v", written)
	}
	// A failed write is retried on the next read, even inside the retry window.
	failWrites = true
	disable := aksmachine.GoalState{DisconnectToleranceEnabled: ptr.To(false)}
	ops.fail = hubRestore
	_ = adapter.ReconcileHub(t.Context(), disable, &State{ActiveMachine: "kube2"}, nil)
	failWrites = false
	_ = adapter.ReconcileHub(t.Context(), disable, &State{ActiveMachine: "kube2"}, nil)
	if len(written) != 3 || written[2].Reason != "DisableFailed" {
		t.Fatalf("failed condition write not retried: %+v", written)
	}
}

func TestHubConditionReasons(t *testing.T) {
	t.Parallel()
	// Error text carrying a URL must not reach the Node.
	detail := errors.New("download https://packages.microsoft.com/x.deb failed")
	for name, tt := range map[string]struct {
		intent     bool
		err        error
		wantStatus corev1.ConditionStatus
		wantReason string
	}{
		"enabled":                       {true, nil, corev1.ConditionTrue, "Enabled"},
		"disabled":                      {false, nil, corev1.ConditionFalse, "Disabled"},
		"download failure":              {true, fmt.Errorf("prepare: %w: %w", yurthub.ErrPackageDownload, detail), corev1.ConditionFalse, "PackageDownloadFailed"},
		"unsupported":                   {true, fmt.Errorf("prepare: %w", yurthub.ErrUnsupportedWorker), corev1.ConditionFalse, "Unsupported"},
		"converge failure keeps hub on": {true, fmt.Errorf("%w: %w", errHubConverge, detail), corev1.ConditionTrue, "Degraded"},
		"enable failure":                {true, detail, corev1.ConditionUnknown, "EnableFailed"},
		"disable failure":               {false, detail, corev1.ConditionUnknown, "DisableFailed"},
	} {
		got := hubCondition(tt.intent, tt.err)
		if got.Status != tt.wantStatus || got.Reason != tt.wantReason || got.Message == "" || len(got.Message) > 100 || strings.Contains(got.Message, "x.deb") {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

func TestHubAdapterHeartbeatEdgeCases(t *testing.T) {
	t.Parallel()
	t.Run("refreshed while waiting to retry", func(t *testing.T) {
		t.Parallel()
		ops := &testHubOperations{fail: hubPrepare, failErr: fmt.Errorf("prepare: %w", yurthub.ErrUnsupportedWorker)}
		adapter, _ := newTestHubAdapter(t, ops)
		clock := time.Unix(1000, 0)
		adapter.now = func() time.Time { return clock }
		node, writes := liveNode(adapter)
		enable := aksmachine.GoalState{DisconnectToleranceEnabled: ptr.To(true)}
		state := &State{ActiveMachine: "kube1"}
		_ = adapter.ReconcileHub(t.Context(), enable, state, node)
		// Past the heartbeat but inside the 15-minute Unsupported retry delay.
		clock = clock.Add(adapter.heartbeat + time.Minute)
		_ = adapter.ReconcileHub(t.Context(), enable, state, node)
		written := *writes
		if len(ops.calls) != 1 || len(written) != 2 || written[1].Reason != "Unsupported" || !written[1].LastHeartbeatTime.Time.Equal(clock) {
			t.Fatalf("calls=%v writes=%+v", ops.calls, written)
		}
	})
	t.Run("future heartbeat is refreshed", func(t *testing.T) {
		t.Parallel()
		adapter, _ := newTestHubAdapter(t, &testHubOperations{})
		clock := time.Unix(1000, 0)
		adapter.now = func() time.Time { return clock }
		node, writes := liveNode(adapter)
		future := metav1.NewTime(clock.Add(time.Hour))
		node.Status.Conditions = []corev1.NodeCondition{{Type: hubConditionType, Status: corev1.ConditionFalse, Reason: "Disabled",
			Message: hubCondition(false, nil).Message, LastHeartbeatTime: future, LastTransitionTime: future}}
		_ = adapter.ReconcileHub(t.Context(), aksmachine.GoalState{DisconnectToleranceEnabled: ptr.To(false)}, &State{ActiveMachine: "kube1"}, node)
		if len(*writes) != 1 || !(*writes)[0].LastHeartbeatTime.Time.Equal(clock) {
			t.Fatalf("future heartbeat not corrected: %+v", *writes)
		}
	})
}
