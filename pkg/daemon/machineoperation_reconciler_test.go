package daemon

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/agent/daemon"
)

func TestHasMachineOperationAPI(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		mapper  meta.RESTMapper
		want    bool
		wantErr bool
	}{
		"present": {
			mapper: machineOperationRESTMapper(),
			want:   true,
		},
		"missing mapping": {
			mapper: meta.NewDefaultRESTMapper([]schema.GroupVersion{}),
		},
		"mapper error": {
			mapper:  errorRESTMapper{err: errors.New("boom")},
			wantErr: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := hasMachineOperationAPI(fake.NewClientBuilder().WithRESTMapper(tt.mapper).Build())
			if tt.wantErr {
				if err == nil {
					t.Fatal("hasMachineOperationAPI error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("hasMachineOperationAPI: %v", err)
			}
			if got != tt.want {
				t.Fatalf("hasMachineOperationAPI = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMachineOperationReconcilerDisableModeSkipsDiscovery(t *testing.T) {
	t.Parallel()

	reconciler, err := machineOperationReconciler(machineOperationReconcilerOptions{
		Client: fake.NewClientBuilder().WithRESTMapper(
			errorRESTMapper{err: errors.New("boom")},
		).Build(),
		Log:                  slog.Default(),
		MachineOperationMode: machineOperationModeDisable,
	})
	if err != nil {
		t.Fatalf("machineOperationReconciler: %v", err)
	}
	if reconciler == nil {
		t.Fatal("machineOperationReconciler returned nil")
	}
}

func machineOperationRESTMapper() meta.RESTMapper {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{machinav1alpha3.GroupVersion})
	mapper.Add(schema.GroupVersionKind{
		Group:   machinav1alpha3.GroupVersion.Group,
		Version: machinav1alpha3.GroupVersion.Version,
		Kind:    "MachineOperation",
	}, meta.RESTScopeRoot)
	return mapper
}

type errorRESTMapper struct {
	err error
}

func (m errorRESTMapper) KindFor(schema.GroupVersionResource) (schema.GroupVersionKind, error) {
	return schema.GroupVersionKind{}, m.err
}

func (m errorRESTMapper) KindsFor(schema.GroupVersionResource) ([]schema.GroupVersionKind, error) {
	return nil, m.err
}

func (m errorRESTMapper) ResourceFor(schema.GroupVersionResource) (schema.GroupVersionResource, error) {
	return schema.GroupVersionResource{}, m.err
}

func (m errorRESTMapper) ResourcesFor(schema.GroupVersionResource) ([]schema.GroupVersionResource, error) {
	return nil, m.err
}

func (m errorRESTMapper) RESTMapping(schema.GroupKind, ...string) (*meta.RESTMapping, error) {
	return nil, m.err
}

func (m errorRESTMapper) RESTMappings(schema.GroupKind, ...string) ([]*meta.RESTMapping, error) {
	return nil, m.err
}

func (m errorRESTMapper) ResourceSingularizer(string) (string, error) {
	return "", m.err
}

func TestMachineOperationHandlersNodeReboot(t *testing.T) {
	t.Parallel()

	operator := &fakeNodeOperator{state: &State{ActiveMachine: "kube1"}}
	store := &fakeMachineOperationStore{}
	target := &machineOperationHandlers{log: slog.Default(), operator: operator}

	if _, err := target.reconcileNodeReboot(t.Context(), store, daemon.MachineOperation{Name: "op1", Kind: machinav1alpha3.OperationNodeReboot}); err != nil {
		t.Fatalf("reconcileNodeReboot: %v", err)
	}
	if !store.inProgress {
		t.Fatal("MarkInProgress was not called")
	}
	if !operator.restarted {
		t.Fatal("RestartNode was not called")
	}
	if store.result.Phase != machinav1alpha3.OperationPhaseComplete {
		t.Fatalf("phase = %s, want %s", store.result.Phase, machinav1alpha3.OperationPhaseComplete)
	}
}

func TestMachineOperationHandlersUnsupportedOperation(t *testing.T) {
	t.Parallel()

	store := &fakeMachineOperationStore{}
	target := &machineOperationHandlers{log: slog.Default(), operator: &fakeNodeOperator{}}

	if _, err := target.unsupportedOperation(t.Context(), store, daemon.MachineOperation{Name: "op1", Kind: machinav1alpha3.OperationAgentReset}); err != nil {
		t.Fatalf("unsupportedOperation: %v", err)
	}
	if store.result.Phase != machinav1alpha3.OperationPhaseFailed {
		t.Fatalf("phase = %s, want %s", store.result.Phase, machinav1alpha3.OperationPhaseFailed)
	}
	if store.result.Reason != "UnsupportedOperation" {
		t.Fatalf("reason = %s, want UnsupportedOperation", store.result.Reason)
	}
}

func TestMachineOperationHandlersAgentReset(t *testing.T) {
	t.Parallel()

	operator := &fakeNodeOperator{state: &State{ActiveMachine: "kube1"}}
	store := &fakeMachineOperationStore{}
	target := &machineOperationHandlers{log: slog.Default(), operator: operator}

	if _, err := target.reconcileAgentReset(t.Context(), store, daemon.MachineOperation{Name: "op1", Kind: machinav1alpha3.OperationAgentReset}); err != nil {
		t.Fatalf("reconcileAgentReset: %v", err)
	}
	if !store.inProgress {
		t.Fatal("MarkInProgress was not called")
	}
	if !operator.reset {
		t.Fatal("ResetNodeRuntime was not called")
	}
	if !operator.cleared {
		t.Fatal("ClearState was not called")
	}
	if !operator.stopped {
		t.Fatal("StopDaemon was not called")
	}
	if store.result.Phase != machinav1alpha3.OperationPhaseComplete {
		t.Fatalf("phase = %s, want %s", store.result.Phase, machinav1alpha3.OperationPhaseComplete)
	}
	if store.result.Reason != "Succeeded" {
		t.Fatalf("reason = %s, want Succeeded", store.result.Reason)
	}
}

type fakeMachineOperationStore struct {
	inProgress bool
	operation  daemon.MachineOperation
	result     daemon.MachineOperationResult[int64]
}

func (f *fakeMachineOperationStore) MarkInProgress(_ context.Context, op daemon.MachineOperation, _ string) error {
	f.inProgress = true
	f.operation = op
	return nil
}

func (f *fakeMachineOperationStore) Finish(_ context.Context, op daemon.MachineOperation, result daemon.MachineOperationResult[int64]) error {
	f.operation = op
	f.result = result
	return nil
}

var _ daemon.MachineOperationStore[int64] = (*fakeMachineOperationStore)(nil)
