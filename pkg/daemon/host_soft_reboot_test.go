package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/AKSFlexNode/pkg/internal/agentdeployment"
	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	agentdaemon "github.com/Azure/unbounded/pkg/agent/daemon"
)

func softOperation(name string) *machinav1alpha3.MachineOperation {
	return &machinav1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: machinav1alpha3.MachineOperationSpec{
			MachineRef:    "machine1",
			OperationKind: machinav1alpha3.OperationHostSoftReboot,
		},
	}
}

func issuedSoftOperation(
	name, machine, bootID string,
) *machinav1alpha3.MachineOperation {
	op := softOperation(name)
	op.Status.Phase = machinav1alpha3.OperationPhaseInProgress
	op.Status.Targets = []machinav1alpha3.MachineOperationTargetStatus{
		{
			MachineRef: machine,
			Phase:      machinav1alpha3.OperationPhaseInProgress,
			BootID:     bootID,
		},
	}

	return op
}

func softClient(objects ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(newScheme()).
		WithStatusSubresource(&machinav1alpha3.MachineOperation{}).
		WithObjects(objects...).Build()
}

func getSoftOperation(
	t *testing.T,
	c client.Reader,
	name string,
) *machinav1alpha3.MachineOperation {
	t.Helper()

	var op machinav1alpha3.MachineOperation
	if err := c.Get(t.Context(), client.ObjectKey{Name: name}, &op); err != nil {
		t.Fatal(err)
	}

	return &op
}

type softHarness struct {
	handlers   *machineOperationHandlers
	reconciler gatedMachineOperationReconciler
	calls      int
}

func newSoftHarness(
	t *testing.T,
	c client.Client,
	reader client.Reader,
) *softHarness {
	t.Helper()

	gate := newStartupGate()
	gate.open()
	h := &softHarness{}
	h.handlers = &machineOperationHandlers{
		log:          discardLogger(),
		client:       c,
		reader:       reader,
		machineName:  "machine1",
		nodeName:     "node1",
		gate:         gate,
		agentUpgrade: &fakeAgentUpgradeExecutor{},
		readBootID:   func() (string, error) { return "boot1", nil },
	}
	h.handlers.rebootHost = func(ctx context.Context, _ *slog.Logger) error {
		h.calls++
		op := getSoftOperation(t, reader, "soft")
		target := operationTargetStatus(&op.Status, "machine1")
		if op.Status.Phase != machinav1alpha3.OperationPhaseInProgress {
			t.Fatalf("reboot before aggregate start: %+v", op.Status)
		}
		if target == nil {
			t.Fatal("reboot before target start")
		}
		if target.Phase != machinav1alpha3.OperationPhaseInProgress {
			t.Fatalf("target phase before reboot = %s", target.Phase)
		}
		if target.BootID != "boot1" {
			t.Fatalf("target boot ID before reboot = %q", target.BootID)
		}

		return nil
	}
	handlers := agentdaemon.MachineOperationHandlers{
		machinav1alpha3.OperationHostSoftReboot: h.handlers.reconcileHostSoftReboot,
	}
	sdk, err := agentdaemon.NewMachinaMachineOperationReconcilerWithReader(c, reader, "machine1", "node1", handlers)
	if err != nil {
		t.Fatal(err)
	}

	h.reconciler = gatedMachineOperationReconciler{
		delegate: &flexMachineOperationReconciler{
			delegate:    sdk,
			handlers:    handlers,
			reader:      reader,
			machineName: "machine1",
			nodeName:    "node1",
		},
		gate: gate,
	}

	return h
}

func TestHostSoftRebootRecordsStatusBeforeCommandAndDefersDuplicates(t *testing.T) {
	t.Parallel()

	c := softClient(softOperation("soft"), softOperation("other"))
	h := newSoftHarness(t, c, c)
	for _, name := range []string{"soft", "soft", "other"} {
		if _, err := h.reconciler.ReconcileMachineOperation(t.Context(), name); err != nil {
			t.Fatal(err)
		}
	}
	if h.calls != 1 {
		t.Fatalf("reboot calls = %d, want 1", h.calls)
	}
	op := getSoftOperation(t, c, "soft")
	condition := apimeta.FindStatusCondition(op.Status.Conditions, "Completed")
	target := operationTargetStatus(&op.Status, "machine1")
	if op.Status.StartedAt == nil {
		t.Fatal("aggregate start timestamp is missing")
	}
	if op.Status.CompletedAt != nil {
		t.Fatalf("aggregate completed early: %v", op.Status.CompletedAt)
	}
	if op.Status.ObservedMachineGeneration != 0 {
		t.Fatalf("aggregate observed generation = %d", op.Status.ObservedMachineGeneration)
	}
	if target == nil {
		t.Fatal("missing local target")
	}
	if target.StartedAt == nil {
		t.Fatal("target start timestamp is missing")
	}
	if target.ObservedGeneration != 0 {
		t.Fatalf("target observed generation = %d", target.ObservedGeneration)
	}
	if condition == nil {
		t.Fatal("missing Completed condition")
	}
	if condition.Status != metav1.ConditionFalse {
		t.Fatalf("condition status = %s", condition.Status)
	}
	if condition.Reason != "InProgress" {
		t.Fatalf("condition reason = %s", condition.Reason)
	}
	if got := getSoftOperation(t, c, "other").Status.Phase; got != "" {
		t.Fatalf("deferred operation phase = %s", got)
	}
}

type trackingActivationLock struct {
	*fakeAgentUpgradeExecutor
	held     bool
	acquired int
	closed   int
}

func (l *trackingActivationLock) Acquire() (io.Closer, error) {
	l.acquired++
	l.held = true
	return l, nil
}

func (l *trackingActivationLock) Close() error {
	l.closed++
	l.held = false
	return nil
}

func TestHostSoftRebootHoldsActivationLock(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		wait       bool
		writeError bool
		commandErr bool
		wantWrites int
		wantCalls  int
		wantErr    bool
	}{
		"out-of-band wait": {
			wait:       true,
			wantWrites: 1,
		},
		"start write error": {
			writeError: true,
			wantWrites: 1,
			wantErr:    true,
		},
		"command error": {
			commandErr: true,
			wantWrites: 2,
			wantCalls:  1,
		},
		"success": {
			wantWrites: 1,
			wantCalls:  1,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			lock := &trackingActivationLock{fakeAgentUpgradeExecutor: &fakeAgentUpgradeExecutor{}}
			objects := []client.Object{softOperation("soft")}
			if tc.wait {
				blocker := softOperation("oob")
				blocker.Spec.OperationKind = machinav1alpha3.OperationHostHardReboot
				objects = append(objects, blocker)
			}
			lists, writes, calls := 0, 0, 0
			c := fake.NewClientBuilder().WithScheme(newScheme()).
				WithStatusSubresource(&machinav1alpha3.MachineOperation{}).
				WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					lists++
					if !lock.held || lock.acquired != 1 || lock.closed != 0 {
						t.Error("live OOB list ran without the activation lock")
					}
					return c.List(ctx, list, opts...)
				},
				SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					writes++
					if !lock.held || lock.closed != 0 {
						t.Error("status write ran without the activation lock")
					}
					if tc.writeError {
						return errors.New("start write failed")
					}
					return c.SubResource(sub).Update(ctx, obj, opts...)
				},
			}).Build()
			h := newSoftHarness(t, c, c)
			h.handlers.agentUpgrade = lock
			h.handlers.rebootHost = func(context.Context, *slog.Logger) error {
				calls++
				if !lock.held || lock.closed != 0 {
					t.Error("command ran without the activation lock")
				}
				if tc.commandErr {
					return errors.New("command failed")
				}
				return nil
			}

			result, err := h.handlers.reconcileHostSoftReboot(t.Context(), &fakeMachineOperationStore{}, agentdaemon.MachineOperation{Name: "soft"})
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error %v", err, tc.wantErr)
			}
			if (result.RequeueAfter == hostSoftRebootRetry) != tc.wait {
				t.Fatalf("requeue = %v, wait = %v", result.RequeueAfter, tc.wait)
			}
			if lists != 1 {
				t.Fatalf("lists = %d, want 1", lists)
			}
			if writes != tc.wantWrites {
				t.Fatalf("writes = %d, want %d", writes, tc.wantWrites)
			}
			if calls != tc.wantCalls {
				t.Fatalf("calls = %d, want %d", calls, tc.wantCalls)
			}
			if lock.acquired != 1 {
				t.Fatalf("lock acquisitions = %d, want 1", lock.acquired)
			}
			if lock.closed != 1 {
				t.Fatalf("lock closes = %d, want 1", lock.closed)
			}
			if lock.held {
				t.Fatal("activation lock still held")
			}
		})
	}
}

func TestHostSoftRebootWriteFailureAndLockContention(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		writeErr, lockErr error
		wantError         bool
		requeue           bool
	}{
		"status failure": {
			writeErr:  errors.New("status unavailable"),
			wantError: true,
		},
		"activation lock held": {
			lockErr: agentbinary.ErrActivationInProgress,
			requeue: true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&machinav1alpha3.MachineOperation{}).
				WithObjects(softOperation("soft")).WithInterceptorFuncs(interceptor.Funcs{
				SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					if tc.writeErr != nil {
						return tc.writeErr
					}
					return c.SubResource(sub).Update(ctx, obj, opts...)
				},
			}).Build()
			h := newSoftHarness(t, c, c)
			h.handlers.agentUpgrade = &fakeAgentUpgradeExecutor{acquireErr: tc.lockErr}
			result, err := h.reconciler.ReconcileMachineOperation(t.Context(), "soft")
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v, want error %v", err, tc.wantError)
			}
			if (result.RequeueAfter > 0) != tc.requeue {
				t.Fatalf("requeue = %v, want requeue %v", result.RequeueAfter, tc.requeue)
			}
			if h.calls != 0 {
				t.Fatalf("reboot calls = %d, want 0", h.calls)
			}
			if h.handlers.gate.stopping.Load() {
				t.Fatal("failure stopped admission")
			}
			if phase := getSoftOperation(t, c, "soft").Status.Phase; phase != "" {
				t.Fatalf("failure changed phase to %s", phase)
			}
		})
	}
}

func TestHostSoftRebootUncertainStatusWriteNeverRunsCommand(t *testing.T) {
	t.Parallel()

	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprintf("canceled=%v", canceled), func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			writes := 0
			c := fake.NewClientBuilder().WithScheme(newScheme()).
				WithStatusSubresource(&machinav1alpha3.MachineOperation{}).
				WithObjects(softOperation("soft")).WithInterceptorFuncs(interceptor.Funcs{
				SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					writes++
					if err := c.SubResource(sub).Update(ctx, obj, opts...); err != nil {
						return err
					}
					if writes == 1 {
						if canceled {
							cancel()
						}
						return errors.New("response lost")
					}
					return nil
				},
			}).Build()
			h := newSoftHarness(t, c, c)
			_, err := h.reconciler.ReconcileMachineOperation(ctx, "soft")
			if err == nil {
				t.Fatal("lost write response returned no error")
			}
			if h.calls != 0 {
				t.Fatalf("reboot calls = %d, want 0", h.calls)
			}
			op := getSoftOperation(t, c, "soft")
			if op.Status.Phase != machinav1alpha3.OperationPhaseInProgress {
				t.Fatalf("aggregate phase = %s", op.Status.Phase)
			}
			if op.Status.Targets[0].Phase != machinav1alpha3.OperationPhaseInProgress {
				t.Fatalf("target phase = %s", op.Status.Targets[0].Phase)
			}
			if writes != 1 {
				t.Fatalf("writes = %d, want 1", writes)
			}

			if _, err := h.reconciler.ReconcileMachineOperation(t.Context(), "soft"); err != nil {
				t.Fatal(err)
			}
			if h.calls != 0 || writes != 1 {
				t.Fatalf("replayed an unacknowledged start: calls = %d, writes = %d", h.calls, writes)
			}

			if err := publishHostSoftReboots(t.Context(), discardLogger(), c, c, "machine1", func() (string, error) { return "boot1", nil }); err != nil {
				t.Fatal(err)
			}
			op = getSoftOperation(t, c, "soft")
			condition := apimeta.FindStatusCondition(op.Status.Conditions, "Completed")
			if op.Status.Phase != machinav1alpha3.OperationPhaseFailed || condition == nil || condition.Reason != "RebootNotObserved" {
				t.Fatalf("startup judgment = %+v", op.Status)
			}
		})
	}
}

func TestHostSoftRebootCommandFailureAndCancellation(t *testing.T) {
	t.Parallel()

	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprintf("canceled=%v", canceled), func(t *testing.T) {
			t.Parallel()
			c := softClient(softOperation("soft"))
			h := newSoftHarness(t, c, c)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			h.handlers.rebootHost = func(context.Context, *slog.Logger) error {
				if canceled {
					cancel()
				}
				return errors.New("reboot failed")
			}
			if _, err := h.reconciler.ReconcileMachineOperation(ctx, "soft"); err != nil {
				t.Fatal(err)
			}
			op := getSoftOperation(t, c, "soft")
			want := machinav1alpha3.OperationPhaseFailed
			if canceled {
				want = machinav1alpha3.OperationPhaseInProgress
			}
			if op.Status.Phase != want {
				t.Fatalf("aggregate phase = %s, want %s", op.Status.Phase, want)
			}
			if op.Status.Targets[0].Phase != want {
				t.Fatalf("target phase = %s, want %s", op.Status.Targets[0].Phase, want)
			}
			if h.handlers.gate.stopping.Load() != canceled {
				t.Fatalf("stopping = %v, want %v", h.handlers.gate.stopping.Load(), canceled)
			}
		})
	}
}

func TestHostSoftRebootCASAndTerminalStates(t *testing.T) {
	t.Parallel()

	for _, phase := range []machinav1alpha3.OperationPhase{machinav1alpha3.OperationPhaseInProgress, machinav1alpha3.OperationPhaseComplete, machinav1alpha3.OperationPhaseFailed} {
		t.Run(string(phase), func(t *testing.T) {
			t.Parallel()
			op := softOperation("soft")
			op.Status.Phase = phase
			c := softClient(op)
			h := newSoftHarness(t, c, c)
			if _, err := h.reconciler.ReconcileMachineOperation(t.Context(), "soft"); err != nil {
				t.Fatal(err)
			}
			if h.calls != 0 {
				t.Fatal("rebooted an already started operation")
			}
		})
	}
	t.Run("conflict then concurrent start", func(t *testing.T) {
		t.Parallel()
		updates := 0
		c := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&machinav1alpha3.MachineOperation{}).
			WithObjects(softOperation("soft")).WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				updates++
				if updates == 1 {
					var latest machinav1alpha3.MachineOperation
					if err := c.Get(ctx, client.ObjectKey{Name: "soft"}, &latest); err != nil {
						return err
					}
					latest.Status.Phase = machinav1alpha3.OperationPhaseInProgress
					if err := c.Status().Update(ctx, &latest); err != nil {
						return err
					}
					return apierrors.NewConflict(schema.GroupResource{Group: machinav1alpha3.GroupVersion.Group, Resource: "machineoperations"}, "soft", errors.New("changed"))
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).Build()
		h := newSoftHarness(t, c, c)
		if _, err := h.reconciler.ReconcileMachineOperation(t.Context(), "soft"); err != nil {
			t.Fatal(err)
		}
		if h.calls != 0 || updates != 1 {
			t.Fatalf("calls = %d updates = %d", h.calls, updates)
		}
	})
}

func TestPublishHostSoftReboots(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, current string
		phase         machinav1alpha3.OperationPhase
		reason        string
	}{
		{
			name:    "changed",
			current: "boot2",
			phase:   machinav1alpha3.OperationPhaseComplete,
			reason:  "Succeeded",
		},
		{
			name:    "same",
			current: "boot1",
			phase:   machinav1alpha3.OperationPhaseFailed,
			reason:  "RebootNotObserved",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			writes := 0
			c := fake.NewClientBuilder().WithScheme(newScheme()).
				WithStatusSubresource(&machinav1alpha3.MachineOperation{}).
				WithObjects(issuedSoftOperation("soft", "machine1", "boot1")).
				WithInterceptorFuncs(interceptor.Funcs{
					SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
						writes++
						return c.SubResource(sub).Update(ctx, obj, opts...)
					},
				}).Build()
			publish := func() error {
				return publishHostSoftReboots(t.Context(), discardLogger(), c, c, "machine1", func() (string, error) { return tc.current, nil })
			}

			if err := publish(); err != nil {
				t.Fatal(err)
			}
			op := getSoftOperation(t, c, "soft")
			condition := apimeta.FindStatusCondition(op.Status.Conditions, "Completed")
			if op.Status.Phase != tc.phase {
				t.Fatalf("aggregate phase = %s, want %s", op.Status.Phase, tc.phase)
			}
			if len(op.Status.Targets) != 1 {
				t.Fatalf("targets = %+v, want one", op.Status.Targets)
			}
			if op.Status.Targets[0].Phase != tc.phase {
				t.Fatalf("target phase = %s, want %s", op.Status.Targets[0].Phase, tc.phase)
			}
			if op.Status.CompletedAt == nil {
				t.Fatal("missing aggregate completion timestamp")
			}
			if op.Status.Targets[0].CompletedAt == nil {
				t.Fatal("missing target completion timestamp")
			}
			if condition == nil {
				t.Fatal("missing completion condition")
			}
			if condition.Reason != tc.reason {
				t.Fatalf("condition reason = %s, want %s", condition.Reason, tc.reason)
			}
			if op.Status.ObservedMachineGeneration != 0 {
				t.Fatalf("observed generation = %d", op.Status.ObservedMachineGeneration)
			}
			if op.Status.Targets[0].BootID != "boot1" {
				t.Fatalf("target boot ID = %q", op.Status.Targets[0].BootID)
			}
			if writes != 1 {
				t.Fatalf("first-pass writes = %d, want 1", writes)
			}

			if err := publish(); err != nil {
				t.Fatal(err)
			}
			after := getSoftOperation(t, c, "soft")
			if writes != 1 {
				t.Fatalf("second pass wrote status: writes = %d", writes)
			}
			if !reflect.DeepEqual(op.Status, after.Status) {
				t.Fatalf("second pass changed status: before = %+v, after = %+v", op.Status, after.Status)
			}
		})
	}
	t.Run("ignore foreign terminal and missing evidence", func(t *testing.T) {
		t.Parallel()
		foreign := issuedSoftOperation("foreign", "other", "boot1")
		missing := issuedSoftOperation("missing", "machine1", "")
		terminal := issuedSoftOperation("terminal", "machine1", "boot1")
		terminal.Status.Phase = machinav1alpha3.OperationPhaseFailed
		other := issuedSoftOperation("other", "machine1", "boot1")
		other.Spec.OperationKind = machinav1alpha3.OperationAgentUpgrade
		c := softClient(foreign, missing, terminal, other)
		if err := publishHostSoftReboots(t.Context(), discardLogger(), c, c, "machine1", func() (string, error) {
			t.Error("read boot ID unnecessarily")
			return "", nil
		}); err != nil {
			t.Fatal(err)
		}
		for _, op := range []*machinav1alpha3.MachineOperation{foreign, missing, terminal, other} {
			if got := getSoftOperation(t, c, op.Name).Status.Phase; got != op.Status.Phase {
				t.Fatalf("%s phase = %s", op.Name, got)
			}
		}
	})
}

func TestPublishHostSoftRebootsRejectsStaleList(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		phase  machinav1alpha3.OperationPhase
		bootID string
	}{
		{
			name:   "terminal target",
			phase:  machinav1alpha3.OperationPhaseComplete,
			bootID: "boot1",
		},
		{
			name:   "different boot evidence",
			phase:  machinav1alpha3.OperationPhaseInProgress,
			bootID: "new-evidence",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			stale := issuedSoftOperation("soft", "machine1", "boot1")
			fresh := stale.DeepCopy()
			fresh.Status.Targets[0].Phase = tc.phase
			fresh.Status.Targets[0].BootID = tc.bootID
			writes, gets := 0, 0
			c := fake.NewClientBuilder().WithScheme(newScheme()).
				WithStatusSubresource(&machinav1alpha3.MachineOperation{}).
				WithObjects(fresh).WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if err := c.List(ctx, list, opts...); err != nil {
						return err
					}
					operations, ok := list.(*machinav1alpha3.MachineOperationList)
					if !ok {
						return fmt.Errorf("unexpected list %T", list)
					}
					operations.Items = []machinav1alpha3.MachineOperation{*stale.DeepCopy()}
					return nil
				},
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					gets++
					return c.Get(ctx, key, obj, opts...)
				},
				SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					writes++
					return c.SubResource(sub).Update(ctx, obj, opts...)
				},
			}).Build()

			if err := publishHostSoftReboots(t.Context(), discardLogger(), c, c, "machine1", func() (string, error) { return "boot2", nil }); err != nil {
				t.Fatal(err)
			}
			if gets != 1 {
				t.Fatalf("fresh GETs = %d, want 1", gets)
			}
			if writes != 0 {
				t.Fatalf("writes after stale list = %d, want 0", writes)
			}
			after := getSoftOperation(t, c, "soft")
			if !reflect.DeepEqual(fresh.Status, after.Status) {
				t.Fatalf("fresh status changed: before = %+v, after = %+v", fresh.Status, after.Status)
			}
		})
	}
}

func TestPublishHostSoftRebootsReadAndWriteErrors(t *testing.T) {
	t.Parallel()

	for _, failure := range []string{"boot", "list", "write"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()
			c := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&machinav1alpha3.MachineOperation{}).
				WithObjects(issuedSoftOperation("soft", "machine1", "boot1")).WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if failure == "list" {
						return errors.New("list failed")
					}
					return c.List(ctx, list, opts...)
				},
				SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					if failure == "write" {
						return errors.New("write failed")
					}
					return c.SubResource(sub).Update(ctx, obj, opts...)
				},
			}).Build()
			err := publishHostSoftReboots(t.Context(), discardLogger(), c, c, "machine1", func() (string, error) {
				if failure == "boot" {
					return "", errors.New("boot failed")
				}
				return "boot2", nil
			})
			if err == nil {
				t.Fatal("publication failure returned no error")
			}
			if phase := getSoftOperation(t, c, "soft").Status.Phase; phase != machinav1alpha3.OperationPhaseInProgress {
				t.Fatalf("phase = %s, want InProgress", phase)
			}
		})
	}
}

func TestOutOfBandHostOperations(t *testing.T) {
	t.Parallel()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node1",
			Labels: map[string]string{
				"rack": "a",
			},
		},
	}
	makeOp := func(name string, kind machinav1alpha3.OperationKind) *machinav1alpha3.MachineOperation {
		op := softOperation(name)
		op.Spec.OperationKind = kind
		return op
	}
	selector := makeOp("selector", machinav1alpha3.OperationHostPowerOff)
	selector.Spec.MachineRef = ""
	selector.Spec.MachineSelector = &metav1.LabelSelector{
		MatchLabels: map[string]string{
			"rack": "a",
		},
	}

	failedMedia := makeOp("replace", machinav1alpha3.OperationHostReplace)
	failedMedia.Status.Phase = machinav1alpha3.OperationPhaseFailed
	failedMedia.Status.Targets = []machinav1alpha3.MachineOperationTargetStatus{
		{
			MachineRef: "machine1",
			Phase:      machinav1alpha3.OperationPhaseFailed,
			BootMedia: &machinav1alpha3.MachineOperationBootMediaStatus{
				ImageWritten: true,
				Released:     true,
				Cleaned:      true,
			},
		},
	}
	confirmedMedia := failedMedia.DeepCopy()
	confirmedMedia.Name = "replace-finished"
	confirmedMedia.Status.Targets[0].Conditions = []metav1.Condition{
		{
			Type:   machinav1alpha3.MachineOperationConditionCloudInitDone,
			Status: metav1.ConditionTrue,
		},
	}
	localTarget := makeOp("local-target", machinav1alpha3.OperationHostPowerOn)
	localTarget.Spec.MachineRef = "other"
	localTarget.Status.Targets = []machinav1alpha3.MachineOperationTargetStatus{
		{
			MachineRef: "machine1",
			Phase:      machinav1alpha3.OperationPhaseInProgress,
		},
	}
	foreignTarget := makeOp("foreign", machinav1alpha3.OperationHostPowerOn)
	foreignTarget.Status.Targets = []machinav1alpha3.MachineOperationTargetStatus{
		{
			MachineRef: "other",
			Phase:      machinav1alpha3.OperationPhaseInProgress,
		},
	}
	completeTarget := makeOp("finished", machinav1alpha3.OperationHostPowerOn)
	completeTarget.Status.Targets = []machinav1alpha3.MachineOperationTargetStatus{
		{
			MachineRef: "machine1",
			Phase:      machinav1alpha3.OperationPhaseComplete,
		},
	}

	tests := map[string]struct {
		op   *machinav1alpha3.MachineOperation
		want bool
	}{
		"explicit reference": {
			op:   makeOp("explicit", machinav1alpha3.OperationHostHardReboot),
			want: true,
		},
		"node selector": {
			op:   selector,
			want: true,
		},
		"allocation": {
			op: makeOp("allocation", machinav1alpha3.OperationHostAllocate),
		},
		"release": {
			op: makeOp("release", machinav1alpha3.OperationHostRelease),
		},
		"local reboot": {
			op: makeOp("soft", machinav1alpha3.OperationHostSoftReboot),
		},
		"failed media": {
			op:   failedMedia,
			want: true,
		},
		"released media with confirmed boot": {
			op: confirmedMedia,
		},
		"local authoritative target in progress": {
			op:   localTarget,
			want: true,
		},
		"foreign authoritative target": {
			op: foreignTarget,
		},
		"complete local target": {
			op: completeTarget,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := softClient(node.DeepCopy(), tc.op)
			got, err := unfinishedOutOfBandHostOperation(t.Context(), c, "machine1", "node1")
			if err != nil || (got != "") != tc.want {
				t.Fatalf("blocker = %q, err = %v", got, err)
			}
		})
	}
	t.Run("oldest stable", func(t *testing.T) {
		t.Parallel()
		a := makeOp("a", machinav1alpha3.OperationHostPowerOn)
		b := makeOp("b", machinav1alpha3.OperationHostPowerOff)
		newer := makeOp("new", machinav1alpha3.OperationHostReplace)
		a.CreationTimestamp = metav1.NewTime(time.Unix(1, 0))
		b.CreationTimestamp = a.CreationTimestamp
		newer.CreationTimestamp = metav1.NewTime(time.Unix(2, 0))
		got, err := unfinishedOutOfBandHostOperation(t.Context(), softClient(b, newer, a), "machine1", "node1")
		if err != nil || got != "a" {
			t.Fatalf("blocker = %q, err = %v", got, err)
		}
	})
}

func TestHostSoftRebootWaitsForOutOfBandCompletion(t *testing.T) {
	t.Parallel()

	oob := softOperation("oob")
	oob.Spec.OperationKind = machinav1alpha3.OperationHostHardReboot
	c := softClient(softOperation("soft"), oob)
	h := newSoftHarness(t, c, c)
	for range 2 {
		result, err := h.reconciler.ReconcileMachineOperation(t.Context(), "soft")
		if err != nil {
			t.Fatal(err)
		}
		if result.RequeueAfter != hostSoftRebootRetry {
			t.Fatalf("requeue = %v", result.RequeueAfter)
		}
		if h.calls != 0 {
			t.Fatalf("reboot calls = %d, want 0", h.calls)
		}
	}
	waiting := getSoftOperation(t, c, "soft")
	if waiting.Status.Phase != machinav1alpha3.OperationPhasePending || waiting.Status.Message != "waiting for out-of-band host operation oob" {
		t.Fatalf("waiting status = %+v", waiting.Status)
	}
	current := getSoftOperation(t, c, "oob")
	current.Status.Phase = machinav1alpha3.OperationPhaseComplete
	if err := c.Status().Update(t.Context(), current); err != nil {
		t.Fatal(err)
	}
	if _, err := h.reconciler.ReconcileMachineOperation(t.Context(), "soft"); err != nil {
		t.Fatal(err)
	}
	if h.calls != 1 {
		t.Fatalf("reboot calls = %d, want 1", h.calls)
	}
}

func TestHostSoftRebootIgnoresStalePendingCache(t *testing.T) {
	t.Parallel()

	for _, phase := range []machinav1alpha3.OperationPhase{machinav1alpha3.OperationPhaseInProgress, machinav1alpha3.OperationPhaseComplete, machinav1alpha3.OperationPhaseFailed} {
		t.Run(string(phase), func(t *testing.T) {
			t.Parallel()

			current := softOperation("soft")
			current.Status.Phase = phase
			api := softClient(current)
			cached := softClient(softOperation("soft"))
			writer := &staleReadClient{Client: api, stale: cached}
			h := newSoftHarness(t, writer, api)
			if _, err := h.reconciler.ReconcileMachineOperation(t.Context(), "soft"); err != nil {
				t.Fatal(err)
			}
			if h.calls != 0 {
				t.Fatal("stale pending status caused a reboot")
			}
		})
	}
}

func TestHostSoftRebootUsesDirectReaderForBlockersAndSelectors(t *testing.T) {
	t.Parallel()

	soft := softOperation("soft")
	soft.Spec.MachineRef = ""
	soft.Spec.MachineSelector = &metav1.LabelSelector{
		MatchLabels: map[string]string{
			"rack": "a",
		},
	}
	oob := softOperation("oob")
	oob.Spec.OperationKind = machinav1alpha3.OperationHostPowerOff
	oob.Spec.MachineRef = ""
	oob.Spec.MachineSelector = &metav1.LabelSelector{
		MatchLabels: map[string]string{
			"rack": "a",
		},
	}
	apiNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node1",
			Labels: map[string]string{
				"rack": "a",
			},
		},
	}
	cachedNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node1",
			Labels: map[string]string{
				"rack": "b",
			},
		},
	}
	api := softClient(soft, oob, apiNode)
	cached := softClient(soft.DeepCopy(), cachedNode)
	// Use a stale read client with writes forwarded to the API client.
	writer := &staleReadClient{Client: api, stale: cached}
	h := newSoftHarness(t, writer, api)
	result, err := h.reconciler.ReconcileMachineOperation(t.Context(), "soft")
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != hostSoftRebootRetry {
		t.Fatalf("requeue = %v", result.RequeueAfter)
	}
	if h.calls != 0 {
		t.Fatalf("reboot calls = %d, want 0", h.calls)
	}
	if got := getSoftOperation(t, api, "soft").Status.Message; !strings.Contains(got, "oob") {
		t.Fatalf("message = %q", got)
	}
}

type staleReadClient struct {
	client.Client
	stale client.Reader
}

func (c *staleReadClient) Get(
	ctx context.Context,
	key client.ObjectKey,
	obj client.Object,
	opts ...client.GetOption,
) error {
	return c.stale.Get(ctx, key, obj, opts...)
}

func (c *staleReadClient) List(
	ctx context.Context,
	list client.ObjectList,
	opts ...client.ListOption,
) error {
	return c.stale.List(ctx, list, opts...)
}

type countRepave struct{ calls int }

func (r *countRepave) SetupController(b *builder.TypedBuilder[agentdaemon.Request]) *builder.TypedBuilder[agentdaemon.Request] {
	return b
}
func (r *countRepave) ReconcileRepave(context.Context, string) (ctrl.Result, error) {
	r.calls++
	return ctrl.Result{}, nil
}

func TestHostSoftRebootDefersRepave(t *testing.T) {
	t.Parallel()

	c := softClient(softOperation("soft"))
	h := newSoftHarness(t, c, c)
	if _, err := h.reconciler.ReconcileMachineOperation(t.Context(), "soft"); err != nil {
		t.Fatal(err)
	}
	r := &countRepave{}
	wrapped := gatedRepaveReconciler{delegate: r, gate: h.handlers.gate}
	if _, err := wrapped.ReconcileRepave(t.Context(), "node-change"); err != nil {
		t.Fatal(err)
	}
	if r.calls != 0 {
		t.Fatal("repave ran during reboot handoff")
	}
}

type countMachineOperations struct{ calls int }

func (r *countMachineOperations) SetupController(b *builder.TypedBuilder[agentdaemon.Request]) *builder.TypedBuilder[agentdaemon.Request] {
	return b
}

func (r *countMachineOperations) ReconcileMachineOperation(context.Context, string) (ctrl.Result, error) {
	r.calls++
	return ctrl.Result{}, nil
}

func TestHostSoftRebootDefersAllAgentOperations(t *testing.T) {
	t.Parallel()

	c := softClient(softOperation("soft"))
	h := newSoftHarness(t, c, c)
	if _, err := h.reconciler.ReconcileMachineOperation(t.Context(), "soft"); err != nil {
		t.Fatal(err)
	}
	delegate := &countMachineOperations{}
	wrapped := gatedMachineOperationReconciler{delegate: delegate, gate: h.handlers.gate}
	for _, name := range []string{"another-host-reboot", "node-reboot", "agent-upgrade", "agent-reset"} {
		if _, err := wrapped.ReconcileMachineOperation(t.Context(), name); err != nil {
			t.Fatal(err)
		}
	}
	if delegate.calls != 0 {
		t.Fatalf("dispatched %d operations during shutdown", delegate.calls)
	}
}

func TestAgentUpgradeHandoffDefersHostSoftReboot(t *testing.T) {
	t.Parallel()

	c := softClient(softOperation("soft"))
	h := newSoftHarness(t, c, c)
	op := agentdaemon.MachineOperation{
		Name: "upgrade",
		Parameters: map[string]string{
			agentdeployment.UpgradeDownloadURLParameter: "https://example.com/agent.tar.gz",
			agentdeployment.UpgradeSHA256Parameter:      strings.Repeat("a", 64),
		},
	}
	if _, err := h.handlers.reconcileAgentUpgrade(t.Context(), &fakeMachineOperationStore{}, op); err != nil {
		t.Fatal(err)
	}
	if _, err := h.reconciler.ReconcileMachineOperation(t.Context(), "soft"); err != nil {
		t.Fatal(err)
	}
	if h.calls != 0 || getSoftOperation(t, c, "soft").Status.Phase != "" {
		t.Fatal("host reboot started during upgrade handoff")
	}
}

func TestAgentContainerSystemctlUsesHostExec(t *testing.T) {
	t.Parallel()

	dockerfile, err := os.ReadFile("../../Dockerfile.aks-flex-node-agent")
	if err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile("../../scripts/agent-image/host-exec")
	if err != nil {
		t.Fatal(err)
	}
	unit, err := os.ReadFile("../internal/agentdeployment/assets/aks-flex-node-agent-container.service")
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"for tool in machinectl systemctl", "ln -s host-exec /opt/aks-flex-node/bin/$tool", "ENV PATH=/opt/aks-flex-node/bin:"} {
		if !strings.Contains(string(dockerfile), text) {
			t.Fatalf("container image missing %q", text)
		}
	}
	if !strings.Contains(string(script), `exec /usr/sbin/chroot "$host_root" /usr/bin/env "$host_path" "$name" "$@"`) {
		t.Fatal("host-exec does not invoke host tools in the host root")
	}
	for _, text := range []string{"--with-ns pid:/proc/1/ns/pid", "--mount type=bind,src=/,dst=/host"} {
		if !strings.Contains(string(unit), text) {
			t.Fatalf("container unit missing %q", text)
		}
	}
}

func TestHostSoftRebootStartupPublicationRetriesBeforeGateOpens(t *testing.T) {
	t.Parallel()

	for _, failure := range []string{"write", "boot"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()

			writes := 0
			c := fake.NewClientBuilder().WithScheme(newScheme()).
				WithStatusSubresource(&machinav1alpha3.MachineOperation{}).
				WithObjects(issuedSoftOperation("soft", "machine1", "boot1")).WithInterceptorFuncs(interceptor.Funcs{
				SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					writes++
					if failure == "write" && writes == 1 {
						return errors.New("temporary status failure")
					}
					return c.SubResource(sub).Update(ctx, obj, opts...)
				},
			}).Build()
			gate := newStartupGate()
			attempts := 0
			openGateAfterAgentUpgradeJudgment(t.Context(), discardLogger(), func(ctx context.Context) error {
				attempts++
				select {
				case <-gate.ready:
					t.Error("gate opened before reboot publication")
				default:
				}
				if attempts == 2 {
					op := getSoftOperation(t, c, "soft")
					if op.Status.Phase != machinav1alpha3.OperationPhaseInProgress {
						t.Fatalf("failed publication changed phase: %+v", op.Status)
					}
					if op.Status.Targets[0].BootID != "boot1" {
						t.Fatalf("failed publication changed boot ID: %+v", op.Status.Targets[0])
					}
				}
				return publishHostSoftReboots(ctx, discardLogger(), c, c, "machine1", func() (string, error) {
					if failure == "boot" && attempts == 1 {
						return "", errors.New("temporary boot read failure")
					}
					return "boot2", nil
				})
			}, func() (*agentdeployment.UpgradeSignal, error) {
				return nil, nil
			}, noHeldAssets, gate.open, time.Millisecond)

			if attempts != 2 {
				t.Fatalf("attempts = %d, want 2", attempts)
			}
			if op := getSoftOperation(t, c, "soft"); op.Status.Phase != machinav1alpha3.OperationPhaseComplete {
				t.Fatalf("status = %+v", op.Status)
			}
			select {
			case <-gate.ready:
			default:
				t.Fatal("gate did not open after publication")
			}
		})
	}
}

func TestHostSoftRebootInitialBootReadFailure(t *testing.T) {
	t.Parallel()

	c := softClient(softOperation("soft"))
	h := newSoftHarness(t, c, c)
	h.handlers.readBootID = func() (string, error) { return "", errors.New("boot failed") }

	if _, err := h.reconciler.ReconcileMachineOperation(t.Context(), "soft"); err != nil {
		t.Fatalf("boot failure publication: %v", err)
	}
	if h.calls != 0 {
		t.Fatalf("reboot calls = %d, want 0", h.calls)
	}
	op := getSoftOperation(t, c, "soft")
	if op.Status.Phase != machinav1alpha3.OperationPhaseFailed {
		t.Fatalf("phase = %s, want Failed", op.Status.Phase)
	}
	condition := apimeta.FindStatusCondition(op.Status.Conditions, "Completed")
	if condition == nil {
		t.Fatal("missing Completed condition")
	}
	if condition.Reason != "ExecutionFailed" {
		t.Fatalf("reason = %s, want ExecutionFailed", condition.Reason)
	}
	if condition.Message != "boot failed" {
		t.Fatalf("message = %q, want boot failed", condition.Message)
	}
}

func TestHostSoftRebootPreflightReadErrors(t *testing.T) {
	t.Parallel()

	for _, failure := range []string{"list", "node"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()

			blocker := softOperation("oob")
			blocker.Spec.OperationKind = machinav1alpha3.OperationHostPowerOff
			blocker.Spec.MachineRef = ""
			blocker.Spec.MachineSelector = &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"rack": "a",
				},
			}
			objects := []client.Object{softOperation("soft")}
			if failure == "node" {
				objects = append(objects, blocker)
			}
			c := fake.NewClientBuilder().WithScheme(newScheme()).
				WithStatusSubresource(&machinav1alpha3.MachineOperation{}).
				WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if failure == "list" {
						return errors.New("list failed")
					}
					return c.List(ctx, list, opts...)
				},
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if failure == "node" && key.Name == "node1" {
						return errors.New("node read failed")
					}
					return c.Get(ctx, key, obj, opts...)
				},
			}).Build()
			h := newSoftHarness(t, c, c)

			_, err := h.reconciler.ReconcileMachineOperation(t.Context(), "soft")
			if h.calls != 0 {
				t.Fatalf("reboot calls = %d, want 0", h.calls)
			}
			op := getSoftOperation(t, c, "soft")
			if err == nil {
				t.Fatal("preflight read error was not returned")
			}
			if !reflect.DeepEqual(op.Status, machinav1alpha3.MachineOperationStatus{}) {
				t.Fatalf("read error changed status: %+v", op.Status)
			}
		})
	}
}

func TestHostSoftRebootFailedPublicationDoesNotReplayCommand(t *testing.T) {
	t.Parallel()

	writes := 0
	publicationErr := errors.New("publication failed")
	c := fake.NewClientBuilder().WithScheme(newScheme()).
		WithStatusSubresource(&machinav1alpha3.MachineOperation{}).
		WithObjects(softOperation("soft")).WithInterceptorFuncs(interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			writes++
			if writes == 2 {
				return publicationErr
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	}).Build()
	h := newSoftHarness(t, c, c)
	h.handlers.rebootHost = func(context.Context, *slog.Logger) error {
		h.calls++
		return errors.New("command failed")
	}

	_, err := h.reconciler.ReconcileMachineOperation(t.Context(), "soft")
	if !errors.Is(err, publicationErr) {
		t.Fatalf("error = %v, want publication error", err)
	}
	if h.handlers.gate.stopping.Load() {
		t.Fatal("command failure left admission stopped")
	}
	if writes != 2 {
		t.Fatalf("writes = %d, want 2", writes)
	}
	if got := getSoftOperation(t, c, "soft").Status.Phase; got != machinav1alpha3.OperationPhaseInProgress {
		t.Fatalf("phase = %s", got)
	}

	if _, err := h.reconciler.ReconcileMachineOperation(t.Context(), "soft"); err != nil {
		t.Fatal(err)
	}
	if h.calls != 1 || writes != 2 {
		t.Fatalf("replayed the failed command: calls = %d, writes = %d", h.calls, writes)
	}
}

func TestFlexOperationWatchUsesLiveIdentityAndLabels(t *testing.T) {
	t.Parallel()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node1",
			Labels: map[string]string{
				"rack": "a",
			},
		},
	}
	op := softOperation("soft")
	op.Spec.MachineRef = ""
	op.Spec.MachineSelector = &metav1.LabelSelector{
		MatchLabels: map[string]string{
			"rack": "a",
		},
	}
	c := softClient(node, op)
	h := newSoftHarness(t, c, c)
	watch := h.reconciler.delegate.(*flexMachineOperationReconciler)
	if len(watch.mapOperation(t.Context(), op)) != 1 {
		t.Fatal("selector operation was not queued")
	}
	if _, err := h.reconciler.ReconcileMachineOperation(t.Context(), "soft"); err != nil {
		t.Fatal(err)
	}
	if h.calls != 1 {
		t.Fatalf("reboot calls = %d, want 1", h.calls)
	}

	foreign := softOperation("foreign")
	foreign.Spec.MachineRef = "node1"
	if len(watch.mapOperation(t.Context(), foreign)) != 0 {
		t.Fatal("node name admitted as machine identity")
	}

	missingNode := softClient(op.DeepCopy())
	matches, err := matchesFlexMachine(t.Context(), missingNode, op, "machine1", "node1")
	if err != nil || matches {
		t.Fatalf("missing Node: match = %v, error = %v", matches, err)
	}
}
