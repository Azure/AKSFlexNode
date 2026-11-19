package yurthub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/nspawn"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

func newTestHubReconciler(t *testing.T, ops *testHubOperations) (*Reconciler, string) {
	t.Helper()
	machinesDir := t.TempDir()
	for _, machine := range []string{"kube1", "kube2"} {
		if err := os.MkdirAll(filepath.Join(machinesDir, machine), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1"}}
	nodes := fake.NewClientset(node).CoreV1().Nodes()
	adapter := NewReconciler(slog.Default(), "node1", "pool", "https://example.com", nodes, 0)
	adapter.operations = func(Worker) (hubTransitionOperations, error) { return ops, nil }
	return adapter, machinesDir
}

func TestNewReconcilerUsesNodeSettings(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset()
	nodes := client.CoreV1().Nodes()
	log := slog.Default()
	r := NewReconciler(log, "node1", "pool", "https://example.com", nodes, time.Minute)

	worker := testWorker("kube1", "/var/lib/machines/kube1")
	ops, err := r.operations(worker)
	if err != nil {
		t.Fatal(err)
	}
	production, ok := ops.(*Operations)
	if !ok {
		t.Fatalf("operations type = %T", ops)
	}
	want := Config{
		Log:       log,
		Worker:    worker,
		NodeName:  "node1",
		NodePool:  "pool",
		ServerURL: "https://example.com",
		Nodes:     nodes,
	}
	got := production.cfg
	if got.Worker.Exec == nil {
		t.Fatal("worker executor was not passed to operations")
	}
	got.Worker.Exec, want.Worker.Exec = nil, nil
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected operation settings: %+v", got)
	}
	if r.heartbeat != time.Minute {
		t.Fatalf("heartbeat = %v", r.heartbeat)
	}
	if got := NewReconciler(log, "node1", "pool", "https://example.com", nodes, 0).heartbeat; got != hubHeartbeatDefault {
		t.Fatalf("default heartbeat = %v", got)
	}
}

func TestHubReconcilerFollowsIntent(t *testing.T) {
	t.Parallel()
	ops := &testHubOperations{}
	adapter, machinesDir := newTestHubReconciler(t, ops)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1"}}

	steps := []struct {
		intent *bool
		want   []hubPhase
	}{
		{intent: ptr.To(false)},
		{intent: ptr.To(true), want: []hubPhase{hubPrepare, hubRoute, hubVerify}},
		{intent: ptr.To(true), want: []hubPhase{hubConverge}},
		{intent: nil},
		{intent: ptr.To(false), want: []hubPhase{hubRestore, hubCleanup, hubStop}},
		{intent: ptr.To(true), want: []hubPhase{hubPrepare, hubRoute, hubVerify}},
	}
	for i, step := range steps {
		ops.calls = nil
		if err := reconcileHub(t, adapter, machinesDir, step.intent, "kube1", node); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if !reflect.DeepEqual(ops.calls, step.want) {
			t.Fatalf("step %d operations = %v, want %v", i, ops.calls, step.want)
		}
	}
	if _, err := os.Stat(filepath.Join(machinesDir, "kube1", hubWorkerJournal)); err != nil {
		t.Fatalf("journal not stored in worker rootfs: %v", err)
	}
}

func testWorker(scope, dir string) Worker {
	return Worker{
		Scope:       scope,
		Root:        nspawn.RootAt(scope, dir),
		Exec:        func(context.Context, ...string) (string, error) { return "", nil },
		KubeletUnit: "kubelet.service",
		Mode:        isolation.ModeNSpawn,
	}
}

func reconcileHub(t *testing.T, adapter *Reconciler, machinesDir string, intent *bool, machine string, node *corev1.Node) error {
	t.Helper()
	var worker Worker
	if machine != "" {
		worker = testWorker(machine, filepath.Join(machinesDir, machine))
	}
	return adapter.Reconcile(t.Context(), intent, worker, node)
}

func TestHubReconcilerRepavedWorkerStartsFresh(t *testing.T) {
	t.Parallel()
	ops := &testHubOperations{}
	adapter, machinesDir := newTestHubReconciler(t, ops)
	goal := ptr.To(true)
	if err := reconcileHub(t, adapter, machinesDir, goal, "kube1", nil); err != nil {
		t.Fatal(err)
	}
	ops.calls = nil
	// A repaved worker has its own rootfs and no Hub yet, so enablement reruns.
	if err := reconcileHub(t, adapter, machinesDir, goal, "kube2", nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ops.calls, []hubPhase{hubPrepare, hubRoute, hubVerify}) {
		t.Fatalf("repaved worker operations = %v", ops.calls)
	}
	if _, err := os.Stat(filepath.Join(machinesDir, "kube2", hubWorkerJournal)); err != nil {
		t.Fatalf("new worker journal missing: %v", err)
	}
}

func TestHubReconcilerUnsupportedWorkerIsNotRetried(t *testing.T) {
	t.Parallel()
	ops := &testHubOperations{fail: hubPrepare, failErr: fmt.Errorf("prepare: %w", ErrUnsupportedWorker)}
	adapter, machinesDir := newTestHubReconciler(t, ops)
	clock := time.Unix(0, 0)
	adapter.now = func() time.Time { return clock }
	state := "kube1"
	for range 2 {
		if err := reconcileHub(t, adapter, machinesDir, ptr.To(true), state, nil); err != nil {
			t.Fatalf("unsupported worker returned an error for retry: %v", err)
		}
	}
	// The second read is inside the retry window, so only one attempt.
	if !reflect.DeepEqual(ops.calls, []hubPhase{hubPrepare}) {
		t.Fatalf("operations = %v, want one prepare attempt", ops.calls)
	}
	clock = clock.Add(hubRetryMax)
	if err := reconcileHub(t, adapter, machinesDir, ptr.To(true), state, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ops.calls, []hubPhase{hubPrepare, hubPrepare}) {
		t.Fatalf("operations = %v, want a retry after the window", ops.calls)
	}
	// Turning DT off still converges through the disable steps.
	ops.calls, ops.fail = nil, ""
	if err := reconcileHub(t, adapter, machinesDir, ptr.To(false), state, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ops.calls, []hubPhase{hubRestore, hubCleanup, hubStop}) {
		t.Fatalf("disable after unsupported = %v", ops.calls)
	}
}

func TestHubReconcilerBacksOffFailures(t *testing.T) {
	t.Parallel()
	ops := &testHubOperations{fail: hubPrepare}
	adapter, machinesDir := newTestHubReconciler(t, ops)
	clock := time.Unix(0, 0)
	adapter.now = func() time.Time { return clock }
	state := "kube1"
	enable := ptr.To(true)
	attempts := func() int { return len(ops.calls) }

	if err := reconcileHub(t, adapter, machinesDir, enable, state, nil); !errors.Is(err, errHubTest) {
		t.Fatalf("first failure = %v", err)
	}
	// Node events inside the window don't retry.
	if err := reconcileHub(t, adapter, machinesDir, enable, state, nil); err != nil || attempts() != 1 {
		t.Fatalf("retried inside the window: %v, attempts=%d", err, attempts())
	}
	// The delay doubles: retry after 1m, then the next is not due at +1m.
	clock = clock.Add(hubRetryMin)
	if err := reconcileHub(t, adapter, machinesDir, enable, state, nil); !errors.Is(err, errHubTest) || attempts() != 2 {
		t.Fatalf("no retry after 1m: %v, attempts=%d", err, attempts())
	}
	clock = clock.Add(hubRetryMin)
	if err := reconcileHub(t, adapter, machinesDir, enable, state, nil); err != nil || attempts() != 2 {
		t.Fatalf("delay did not double: %v, attempts=%d", err, attempts())
	}
	// A changed intent is acted on immediately.
	ops.fail = ""
	if err := reconcileHub(t, adapter, machinesDir, ptr.To(false), state, nil); err != nil || attempts() != 5 {
		t.Fatalf("changed intent waited for the retry window: %v, calls=%v", err, ops.calls)
	}
	// Success clears the retry state.
	if err := reconcileHub(t, adapter, machinesDir, enable, state, nil); err != nil || attempts() != 8 {
		t.Fatalf("enable after success did not run: %v, calls=%v", err, ops.calls)
	}
}

func TestHubRetryRunsImmediatelyForChangedWorker(t *testing.T) {
	t.Parallel()
	ops := &testHubOperations{fail: hubPrepare}
	adapter, machinesDir := newTestHubReconciler(t, ops)
	adapter.now = func() time.Time { return time.Unix(0, 0) }

	if err := reconcileHub(t, adapter, machinesDir, ptr.To(true), "kube1", nil); !errors.Is(err, errHubTest) {
		t.Fatalf("first failure = %v", err)
	}
	if err := reconcileHub(t, adapter, machinesDir, ptr.To(true), "kube2", nil); !errors.Is(err, errHubTest) {
		t.Fatalf("new worker did not run immediately: %v", err)
	}
	if !reflect.DeepEqual(ops.calls, []hubPhase{hubPrepare, hubPrepare}) {
		t.Fatalf("operations = %v", ops.calls)
	}
}

func TestHubReconcilerFalseWithoutWorkerIsNoop(t *testing.T) {
	t.Parallel()
	ops := &testHubOperations{}
	adapter, machinesDir := newTestHubReconciler(t, ops)
	if err := reconcileHub(t, adapter, machinesDir, ptr.To(false), "", nil); err != nil || len(ops.calls) != 0 {
		t.Fatalf("false without an active worker = %v, %v", ops.calls, err)
	}
}

// TestHubReconcilerFalseWithoutJournalSkipsOperations: a worker that never started a
// transition reports Disabled without building operations (which validate settings
// only enabling needs) or writing anything, even if those settings are invalid or the
// worker rootfs is missing.
func TestHubReconcilerFalseWithoutJournalSkipsOperations(t *testing.T) {
	t.Parallel()
	for name, machine := range map[string]string{"existing worker": "kube1", "missing worker rootfs": "kube2"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			adapter, machinesDir := newTestHubReconciler(t, &testHubOperations{})
			if machine == "kube2" {
				if err := os.RemoveAll(filepath.Join(machinesDir, machine)); err != nil {
					t.Fatal(err)
				}
			}
			built := 0
			adapter.operations = func(Worker) (hubTransitionOperations, error) {
				built++
				return nil, errors.New("invalid hub settings")
			}
			node, writes := liveNode(adapter)
			disable := ptr.To(false)
			for range 2 {
				if err := reconcileHub(t, adapter, machinesDir, disable, machine, node); err != nil {
					t.Fatal(err)
				}
			}
			if built != 0 {
				t.Fatalf("built hub operations %d times for a DT-off worker", built)
			}
			if len(*writes) != 1 || (*writes)[0].Reason != hubReasonDisabled {
				t.Fatalf("condition writes = %+v, want one Disabled", *writes)
			}
			if _, err := os.Stat(filepath.Join(machinesDir, machine, hubWorkerJournal)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("DT-off worker got a journal: %v", err)
			}
		})
	}
}

func TestHubReconcilerRequiresWorkerForEnable(t *testing.T) {
	t.Parallel()
	ops := &testHubOperations{}
	adapter, machinesDir := newTestHubReconciler(t, ops)
	err := reconcileHub(t, adapter, machinesDir, ptr.To(true), "", nil)
	if err == nil || len(ops.calls) != 0 {
		t.Fatalf("missing worker ran operations: %v, %v", ops.calls, err)
	}
}

// liveNode returns a Node whose status reflects condition writes, like the API.
func liveNode(adapter *Reconciler) (*corev1.Node, *[]corev1.NodeCondition) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1"}}
	var written []corev1.NodeCondition
	adapter.setCondition = func(_ context.Context, c corev1.NodeCondition) error {
		written = append(written, c)
		node.Status.Conditions = []corev1.NodeCondition{c}
		return nil
	}
	return node, &written
}

func TestHubReconcilerReportsNodeCondition(t *testing.T) {
	t.Parallel()
	ops := &testHubOperations{}
	adapter, machinesDir := newTestHubReconciler(t, ops)
	clock := time.Unix(0, 0)
	adapter.now = func() time.Time { return clock }
	node, writes := liveNode(adapter)
	state := "kube1"
	reconcile := func(intent bool) {
		t.Helper()
		_ = reconcileHub(t, adapter, machinesDir, ptr.To(intent), state, node)
	}
	steps := []struct {
		intent     bool
		fail       error
		wantStatus corev1.ConditionStatus
		wantReason string
	}{
		{intent: true, fail: fmt.Errorf("prepare: %w", ErrPackageDownload), wantStatus: corev1.ConditionFalse, wantReason: "PackageDownloadFailed"},
		{intent: true, fail: fmt.Errorf("prepare: %w", ErrUnsupportedWorker), wantStatus: corev1.ConditionFalse, wantReason: "Unsupported"},
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
	// A steady Disabled condition gets no heartbeat, so DT-off nodes add no writes.
	clock = clock.Add(adapter.heartbeat)
	reconcile(false)
	if written = *writes; len(written) != len(steps) {
		t.Fatalf("Disabled condition refreshed: %+v", written[len(steps):])
	}
	// Enabled is refreshed after the heartbeat interval, keeping its transition time.
	reconcile(true)
	clock = clock.Add(adapter.heartbeat)
	reconcile(true)
	written = *writes
	last, prev := written[len(written)-1], written[len(written)-2]
	if len(written) != len(steps)+2 || last.Reason != "Enabled" || !last.LastHeartbeatTime.After(prev.LastHeartbeatTime.Time) || !last.LastTransitionTime.Equal(&prev.LastTransitionTime) {
		t.Fatalf("heartbeat not refreshed correctly: %+v", written)
	}
}

// The live Node is the source of truth: a removed or edited condition is
// restored, and an agent restart keeps the existing transition time.
func TestHubReconcilerConditionFollowsLiveNode(t *testing.T) {
	t.Parallel()
	adapter, machinesDir := newTestHubReconciler(t, &testHubOperations{})
	clock := time.Unix(1000, 0)
	adapter.now = func() time.Time { return clock }
	node, writes := liveNode(adapter)
	enable := ptr.To(true)
	state := "kube1"
	_ = reconcileHub(t, adapter, machinesDir, enable, state, node)
	// Someone deletes the condition.
	node.Status.Conditions = nil
	clock = clock.Add(time.Minute)
	_ = reconcileHub(t, adapter, machinesDir, enable, state, node)
	if len(*writes) != 2 {
		t.Fatalf("removed condition not restored: %+v", *writes)
	}
	// A restarted agent (fresh adapter) sees an unchanged, fresh condition: no write.
	restarted, restartedDir := newTestHubReconciler(t, &testHubOperations{})
	restarted.now = adapter.now
	restartedWrites := 0
	restarted.setCondition = func(context.Context, corev1.NodeCondition) error {
		restartedWrites++
		return nil
	}
	_ = reconcileHub(t, restarted, restartedDir, enable, state, node)
	if restartedWrites != 0 {
		t.Fatal("restart rewrote an unchanged condition")
	}
}

func TestHubReconcilerConditionWriteFailureDoesNotFailReconcile(t *testing.T) {
	t.Parallel()
	adapter, machinesDir := newTestHubReconciler(t, &testHubOperations{})
	writes := 0
	adapter.setCondition = func(context.Context, corev1.NodeCondition) error {
		writes++
		return errors.New("forbidden")
	}
	goal := ptr.To(true)
	for range 2 {
		if err := reconcileHub(t, adapter, machinesDir, goal, "kube1", nil); err != nil {
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

func TestHubReconcilerConditionNewNodeAndRetriedWrites(t *testing.T) {
	t.Parallel()
	ops := &testHubOperations{}
	adapter, machinesDir := newTestHubReconciler(t, ops)
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
	enable := ptr.To(true)
	if err := reconcileHub(t, adapter, machinesDir, enable, "kube1", nil); err != nil {
		t.Fatal(err)
	}
	// A repaved worker registers a new Node object without the condition.
	if err := reconcileHub(t, adapter, machinesDir, enable, "kube2", &corev1.Node{}); err != nil {
		t.Fatal(err)
	}
	if len(written) != 2 || written[1].Reason != "Enabled" {
		t.Fatalf("new worker condition not written: %+v", written)
	}
	// A failed write is retried on the next read, even inside the retry window.
	failWrites = true
	disable := ptr.To(false)
	ops.fail = hubRestore
	_ = reconcileHub(t, adapter, machinesDir, disable, "kube2", nil)
	failWrites = false
	_ = reconcileHub(t, adapter, machinesDir, disable, "kube2", nil)
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
		"download failure":              {true, fmt.Errorf("prepare: %w: %w", ErrPackageDownload, detail), corev1.ConditionFalse, "PackageDownloadFailed"},
		"unsupported":                   {true, fmt.Errorf("prepare: %w", ErrUnsupportedWorker), corev1.ConditionFalse, "Unsupported"},
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

func TestHubReconcilerHeartbeatEdgeCases(t *testing.T) {
	t.Parallel()
	t.Run("refreshed while waiting to retry", func(t *testing.T) {
		t.Parallel()
		ops := &testHubOperations{fail: hubPrepare, failErr: fmt.Errorf("prepare: %w", ErrUnsupportedWorker)}
		adapter, machinesDir := newTestHubReconciler(t, ops)
		clock := time.Unix(1000, 0)
		adapter.now = func() time.Time { return clock }
		node, writes := liveNode(adapter)
		enable := ptr.To(true)
		state := "kube1"
		_ = reconcileHub(t, adapter, machinesDir, enable, state, node)
		// Past the heartbeat but inside the 15-minute Unsupported retry delay.
		clock = clock.Add(adapter.heartbeat + time.Minute)
		_ = reconcileHub(t, adapter, machinesDir, enable, state, node)
		written := *writes
		if len(ops.calls) != 1 || len(written) != 2 || written[1].Reason != "Unsupported" || !written[1].LastHeartbeatTime.Time.Equal(clock) {
			t.Fatalf("calls=%v writes=%+v", ops.calls, written)
		}
	})
	t.Run("future heartbeat is refreshed", func(t *testing.T) {
		t.Parallel()
		adapter, machinesDir := newTestHubReconciler(t, &testHubOperations{})
		clock := time.Unix(1000, 0)
		adapter.now = func() time.Time { return clock }
		node, writes := liveNode(adapter)
		enable := ptr.To(true)
		state := "kube1"
		_ = reconcileHub(t, adapter, machinesDir, enable, state, node)
		*writes = nil
		future := metav1.NewTime(clock.Add(time.Hour))
		node.Status.Conditions[0].LastHeartbeatTime, node.Status.Conditions[0].LastTransitionTime = future, future
		_ = reconcileHub(t, adapter, machinesDir, enable, state, node)
		if len(*writes) != 1 || !(*writes)[0].LastHeartbeatTime.Time.Equal(clock) {
			t.Fatalf("future heartbeat not corrected: %+v", *writes)
		}
	})
}
