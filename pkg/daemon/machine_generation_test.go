package daemon

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	agentdaemon "github.com/Azure/unbounded/pkg/agent/daemon"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type generationReader struct {
	client.Reader
	get func(context.Context, client.ObjectKey, client.Object) error
}

func (r generationReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	return r.get(ctx, key, obj)
}

func TestOperationGenerationReadErrors(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		err     error
		wantErr bool
	}{
		"absent CR":  {err: apierrors.NewNotFound(schema.GroupResource{Group: machinav1alpha3.GroupVersion.Group, Resource: "machines"}, "worker")},
		"absent API": {err: &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: machinav1alpha3.GroupVersion.Group, Kind: "Machine"}}},
		"forbidden":  {err: apierrors.NewForbidden(schema.GroupResource{Resource: "machines"}, "worker", errors.New("denied")), wantErr: true},
		"transient":  {err: apierrors.NewServiceUnavailable("retry"), wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, kind := range []machinav1alpha3.OperationKind{machinav1alpha3.OperationNodeReboot, machinav1alpha3.OperationAgentReset, machinav1alpha3.OperationAgentUpgrade} {
				t.Run(string(kind), func(t *testing.T) {
					t.Parallel()
					reader := generationReader{get: func(_ context.Context, key client.ObjectKey, obj client.Object) error {
						if key.Name != "worker" {
							t.Fatalf("read name = %q", key.Name)
						}
						if _, ok := obj.(*machinav1alpha3.Machine); !ok {
							t.Fatalf("unexpected read %T", obj)
						}
						return tt.err
					}}
					operator := &fakeNodeOperator{}
					h := &machineOperationHandlers{reader: reader, machineName: "worker", log: slog.Default(), operator: operator}
					store := &fakeMachineOperationStore{}
					op := agentdaemon.MachineOperation{Name: "op", Kind: kind}
					var err error
					switch kind {
					case machinav1alpha3.OperationNodeReboot:
						_, err = h.reconcileNodeReboot(t.Context(), store, op)
					case machinav1alpha3.OperationAgentReset:
						_, err = h.reconcileAgentReset(t.Context(), store, op)
					case machinav1alpha3.OperationAgentUpgrade:
						executor := &hostAgentUpgradeExecutor{
							machineReader: reader, machineName: "worker",
							state:   &fakeNodeOperator{state: &State{ActiveMachine: "kube1"}},
							signals: agentUpgradeSignalStore{path: filepath.Join(t.TempDir(), "signal.json")},
						}
						var generation int64
						generation, err = executor.RecordPending(t.Context(), "op")
						if generation != 0 {
							t.Fatalf("generation = %d, want omitted", generation)
						}
						signal, readErr := executor.signals.read()
						if readErr != nil || (signal != nil) == tt.wantErr {
							t.Fatalf("signal = %#v, error = %v", signal, readErr)
						}
					}
					if (err != nil) != tt.wantErr {
						t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
					}
					if tt.wantErr && (operator.restarted || operator.reset || store.inProgress) {
						t.Fatal("read failure executed operation")
					}
					if store.result.ObservedMachineGeneration != 0 {
						t.Fatal("absent Machine generation was not omitted")
					}
				})
			}
		})
	}
}

type generationChangingStore struct {
	fakeMachineOperationStore
	onStart func()
}

func (s *generationChangingStore) MarkInProgress(ctx context.Context, op agentdaemon.MachineOperation, message string) error {
	s.onStart()
	return s.fakeMachineOperationStore.MarkInProgress(ctx, op, message)
}

func TestOperationCapturesGenerationBeforeExecutionAndRetriesResult(t *testing.T) {
	t.Parallel()
	for _, kind := range []machinav1alpha3.OperationKind{machinav1alpha3.OperationNodeReboot, machinav1alpha3.OperationAgentReset} {
		for _, fail := range []bool{false, true} {
			name := string(kind) + "/success"
			if fail {
				name = string(kind) + "/failure"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				generation := int64(7)
				reads := 0
				reader := generationReader{get: func(_ context.Context, _ client.ObjectKey, obj client.Object) error {
					reads++
					obj.SetGeneration(generation)
					return nil
				}}
				operator := &fakeNodeOperator{}
				phase := machinav1alpha3.OperationPhaseComplete
				if fail {
					phase = machinav1alpha3.OperationPhaseFailed
					operator.restartErr = errors.New("restart failed")
					operator.resetErr = errors.New("reset failed")
				}
				store := &generationChangingStore{onStart: func() {
					if reads != 1 {
						t.Fatal("generation not captured before InProgress")
					}
					generation = 8
				}}
				store.finishErr = errors.New("status unavailable")
				h := &machineOperationHandlers{reader: reader, machineName: "worker", log: slog.Default(), operator: operator}
				handler := h.reconcileNodeReboot
				if kind == machinav1alpha3.OperationAgentReset {
					handler = h.reconcileAgentReset
				}
				op := agentdaemon.MachineOperation{Name: "op", Kind: kind}
				if _, err := handler(t.Context(), store, op); err == nil {
					t.Fatal("expected status write error")
				}
				store.finishErr = nil
				operator.restarted, operator.reset = false, false
				if _, err := handler(t.Context(), store, op); err != nil {
					t.Fatalf("retry: %v", err)
				}
				if reads != 1 || store.result.ObservedMachineGeneration != 7 || store.result.Phase != phase {
					t.Fatalf("reads = %d, result = %#v", reads, store.result)
				}
				if operator.restarted || operator.reset {
					t.Fatal("terminal status retry repeated host operation")
				}
			})
		}
	}
}

func TestUpgradeCapturesGenerationOnceInDurableSignal(t *testing.T) {
	t.Parallel()
	machine := &machinav1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "worker", Generation: 7}}
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(machine).Build()
	executor := &hostAgentUpgradeExecutor{
		machineReader: c, machineName: "worker",
		state:   &fakeNodeOperator{state: &State{ActiveMachine: "kube2"}},
		signals: agentUpgradeSignalStore{path: filepath.Join(t.TempDir(), "signal.json")},
	}
	if generation, err := executor.RecordPending(t.Context(), "op"); err != nil || generation != 7 {
		t.Fatalf("RecordPending = %d, %v", generation, err)
	}
	machine.Generation = 8
	if err := c.Update(t.Context(), machine); err != nil {
		t.Fatal(err)
	}
	executor.machineReader = generationReader{get: func(context.Context, client.ObjectKey, client.Object) error {
		t.Fatal("duplicate or recovery must not reread Machine")
		return nil
	}}
	if generation, err := executor.RecordPending(t.Context(), "op"); !errors.Is(err, errAgentUpgradeAlreadyPending) || generation != 7 {
		t.Fatalf("duplicate RecordPending = %d, %v", generation, err)
	}
	if _, err := executor.RecordPending(t.Context(), "other"); err == nil || !strings.Contains(err.Error(), "pending") {
		t.Fatalf("competing operation error = %v", err)
	}
	if err := executor.RecordFailure("interrupted before staging"); err != nil {
		t.Fatal(err)
	}
	signal, err := executor.signals.read()
	if err != nil || signal.ObservedMachineGeneration != 7 {
		t.Fatalf("durable recovery signal = %#v, %v", signal, err)
	}
}

func TestUpgradeExecutionFailureReportsCapturedGeneration(t *testing.T) {
	t.Parallel()
	for name, executor := range map[string]*fakeAgentUpgradeExecutor{
		"stage":   {generation: 7, stageErr: errors.New("stage failed")},
		"restart": {generation: 7, restartErr: errors.New("restart failed")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := &machineOperationHandlers{log: slog.Default(), agentUpgrade: executor}
			store := &fakeMachineOperationStore{finishErr: errors.New("status unavailable")}
			op := agentdaemon.MachineOperation{Name: "op", Parameters: map[string]string{agentUpgradeDownloadURLParameter: "https://example.com/agent.tar.gz"}}
			if _, err := h.reconcileAgentUpgrade(t.Context(), store, op); err == nil {
				t.Fatal("expected status write error")
			}
			executor.generation = 8
			executor.pending, executor.staged, executor.restarted = false, false, false
			store.finishErr = nil
			if _, err := h.reconcileAgentUpgrade(t.Context(), store, op); err != nil {
				t.Fatal(err)
			}
			if store.result.ObservedMachineGeneration != 7 || store.result.Phase != machinav1alpha3.OperationPhaseFailed {
				t.Fatalf("result = %#v", store.result)
			}
			if executor.pending || executor.staged || executor.restarted {
				t.Fatal("result retry executed upgrade")
			}
		})
	}
}

func TestReconcilerPublishesGenerationAndSkipsCompletedOperation(t *testing.T) {
	t.Parallel()
	machine := &machinav1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "arm-worker", Generation: 7}}
	op := &machinav1alpha3.MachineOperation{ObjectMeta: metav1.ObjectMeta{Name: "op"}}
	op.Spec.OperationKind = machinav1alpha3.OperationNodeReboot
	op.Spec.MachineRef = machine.Name
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithRESTMapper(machineOperationRESTMapper()).
		WithObjects(machine, op).WithStatusSubresource(op).Build()
	operator := &fakeNodeOperator{}
	r, err := machineOperationReconciler(machineOperationReconcilerOptions{
		Client: c, Reader: c, Log: slog.Default(), NodeName: "node",
		AKSMachineName: machine.Name, Operator: operator, AgentUpgrade: &fakeAgentUpgradeExecutor{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReconcileMachineOperation(t.Context(), op.Name); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(op), op); err != nil {
		t.Fatal(err)
	}
	if op.Status.ObservedMachineGeneration != 7 || op.Status.Phase != machinav1alpha3.OperationPhaseComplete {
		t.Fatalf("status = %#v", op.Status)
	}
	operator.restarted = false
	if _, err := r.ReconcileMachineOperation(t.Context(), op.Name); err != nil {
		t.Fatal(err)
	}
	if operator.restarted {
		t.Fatal("completed operation executed again")
	}
}
