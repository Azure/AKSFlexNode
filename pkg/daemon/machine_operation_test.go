package daemon

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/agent/daemon"
)

func TestHasMachineOperationAPI(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		resources *metav1.APIResourceList
		err       error
		want      bool
		wantErr   bool
	}{
		"present": {
			resources: &metav1.APIResourceList{APIResources: []metav1.APIResource{{Name: machineOperationResource}}},
			want:      true,
		},
		"missing resource": {
			resources: &metav1.APIResourceList{APIResources: []metav1.APIResource{{Name: "machines"}}},
		},
		"missing api group": {
			err: apierrors.NewNotFound(schema.GroupResource{Group: machinav1alpha3.GroupVersion.Group, Resource: machineOperationResource}, ""),
		},
		"discovery error": {
			err:     errors.New("boom"),
			wantErr: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := hasMachineOperationAPI(fakeDiscoveryClient{resources: tt.resources, err: tt.err})
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

func TestMachineOperationTargetNodeReboot(t *testing.T) {
	t.Parallel()

	operator := &fakeNodeOperator{state: &State{ActiveMachine: "kube1"}}
	store := &fakeMachineOperationStore{}
	target := &machineOperationTarget{log: slog.Default(), operator: operator}

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

func TestMachineOperationTargetUnsupportedOperation(t *testing.T) {
	t.Parallel()

	store := &fakeMachineOperationStore{}
	target := &machineOperationTarget{log: slog.Default(), operator: &fakeNodeOperator{}}

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

type fakeDiscoveryClient struct {
	resources *metav1.APIResourceList
	err       error
}

func (f fakeDiscoveryClient) ServerResourcesForGroupVersion(string) (*metav1.APIResourceList, error) {
	return f.resources, f.err
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
