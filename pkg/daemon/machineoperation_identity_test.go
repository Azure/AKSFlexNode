package daemon

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestPendingOperationResultIdentity(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		kind                            machinav1alpha3.OperationKind
		change                          string
		resetFails                      bool
		wantReset, wantReboot, wantStop bool
	}{
		"same UID reboot retry":                        {kind: machinav1alpha3.OperationNodeReboot},
		"same UID reset retry stops after publication": {kind: machinav1alpha3.OperationAgentReset, wantStop: true},
		"same UID failed reset retry does not stop":    {kind: machinav1alpha3.OperationAgentReset, resetFails: true},
		"replacement reboot must execute and not stop": {kind: machinav1alpha3.OperationAgentReset, change: "replace", wantReboot: true},
		"replacement reset must execute before stop":   {kind: machinav1alpha3.OperationNodeReboot, change: "replace", wantReset: true, wantStop: true},
		"missing object discards reset result":         {kind: machinav1alpha3.OperationAgentReset, change: "delete"},
		"terminal object discards reset result":        {kind: machinav1alpha3.OperationAgentReset, change: "terminal"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			failFinish := true
			r, c, operator, op, machine := newIdentityTestReconciler(t, tt.kind, func(context.Context, client.Client, *machinav1alpha3.MachineOperation) error {
				if failFinish {
					return errors.New("terminal status unavailable")
				}
				return nil
			})
			if tt.resetFails {
				operator.resetErr = errors.New("reset failed")
			}
			if _, err := r.ReconcileMachineOperation(t.Context(), op.Name); err == nil {
				t.Fatal("expected terminal status failure")
			}
			pending, ok := r.handlers.pendingResults[op.Name]
			if !ok || pending.uid != "original" || pending.result.ObservedMachineGeneration != 7 {
				t.Fatalf("pending result = %#v, present = %v", pending, ok)
			}
			if operator.stopped {
				t.Fatal("stopped before successful terminal publication")
			}
			operator.reset, operator.restarted = false, false
			failFinish = false
			machine.Generation = 8
			if err := c.Update(t.Context(), machine); err != nil {
				t.Fatal(err)
			}
			switch tt.change {
			case "replace", "delete":
				if err := c.Delete(t.Context(), op); err != nil {
					t.Fatal(err)
				}
				if tt.change == "replace" {
					op = op.DeepCopy()
					op.UID = "replacement"
					op.ResourceVersion = ""
					if tt.kind == machinav1alpha3.OperationNodeReboot {
						op.Spec.OperationKind = machinav1alpha3.OperationAgentReset
					} else {
						op.Spec.OperationKind = machinav1alpha3.OperationNodeReboot
					}
					if err := c.Create(t.Context(), op); err != nil {
						t.Fatal(err)
					}
				}
			case "terminal":
				if err := c.Get(t.Context(), client.ObjectKeyFromObject(op), op); err != nil {
					t.Fatal(err)
				}
				op.Status.Phase = machinav1alpha3.OperationPhaseFailed
				op.Status.Message = "completed elsewhere"
				if err := c.Status().Update(t.Context(), op); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := r.ReconcileMachineOperation(t.Context(), op.Name); err != nil {
				t.Fatalf("retry: %v", err)
			}
			if operator.reset != tt.wantReset || operator.restarted != tt.wantReboot || operator.stopped != tt.wantStop {
				t.Fatalf("reset/reboot/stop = %v/%v/%v, want %v/%v/%v", operator.reset, operator.restarted, operator.stopped, tt.wantReset, tt.wantReboot, tt.wantStop)
			}
			if len(r.handlers.pendingResults) != 0 {
				t.Fatalf("cached result not cleared: %#v", r.handlers.pendingResults)
			}
			if tt.change == "delete" {
				return
			}
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(op), op); err != nil {
				t.Fatal(err)
			}
			wantGeneration := int64(7)
			switch tt.change {
			case "replace":
				wantGeneration = 8
			case "terminal":
				wantGeneration = 0
				if op.Status.Message != "completed elsewhere" {
					t.Fatalf("terminal result overwritten: %#v", op.Status)
				}
			}
			if op.Status.ObservedMachineGeneration != wantGeneration {
				t.Fatalf("generation = %d, want %d", op.Status.ObservedMachineGeneration, wantGeneration)
			}
		})
	}
}

func TestReplacementRacingTerminalRetryIsNotUpdatedOrStopped(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"replace", "terminal", "delete"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			terminalAttempts := 0
			r, c, operator, op, _ := newIdentityTestReconciler(t, machinav1alpha3.OperationAgentReset,
				func(ctx context.Context, c client.Client, result *machinav1alpha3.MachineOperation) error {
					terminalAttempts++
					if terminalAttempts == 1 {
						return errors.New("terminal status unavailable")
					}
					if result.UID != "original" || result.ResourceVersion == "" {
						t.Fatalf("status update lacks original identity/version: %#v", result.ObjectMeta)
					}
					var current machinav1alpha3.MachineOperation
					if err := c.Get(ctx, client.ObjectKeyFromObject(result), &current); err != nil {
						t.Fatal(err)
					}
					switch change {
					case "replace", "delete":
						if err := c.Delete(ctx, &current); err != nil {
							t.Fatal(err)
						}
						if change == "replace" {
							replacement := &machinav1alpha3.MachineOperation{ObjectMeta: metav1.ObjectMeta{Name: result.Name, UID: "replacement"}}
							replacement.Spec.OperationKind = machinav1alpha3.OperationNodeReboot
							if err := c.Create(ctx, replacement); err != nil {
								t.Fatal(err)
							}
						}
					case "terminal":
						current.Status.Phase = machinav1alpha3.OperationPhaseFailed
						current.Status.Message = "completed elsewhere"
						if err := c.Status().Update(ctx, &current); err != nil {
							t.Fatal(err)
						}
					}
					// Model the API server's optimistic-concurrency rejection
					// after the shared store read but before its status Update.
					return apierrors.NewConflict(schema.GroupResource{Group: machinav1alpha3.GroupVersion.Group, Resource: "machineoperations"}, result.Name, errors.New("object changed"))
				})
			if _, err := r.ReconcileMachineOperation(t.Context(), op.Name); err == nil {
				t.Fatal("expected first terminal status failure")
			}
			operator.reset = false
			if _, err := r.ReconcileMachineOperation(t.Context(), op.Name); !errors.Is(err, errMachineOperationNoLongerCurrent) {
				t.Fatalf("retry error = %v, want stale identity", err)
			}
			if terminalAttempts != 2 || operator.reset || operator.restarted || operator.stopped {
				t.Fatalf("attempts = %d, operator = %#v", terminalAttempts, operator)
			}
			if len(r.handlers.pendingResults) != 0 {
				t.Fatal("stale cached result retained")
			}
			var latest machinav1alpha3.MachineOperation
			err := c.Get(t.Context(), client.ObjectKeyFromObject(op), &latest)
			if change == "delete" {
				if !apierrors.IsNotFound(err) {
					t.Fatalf("get deleted operation: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if latest.Status.ObservedMachineGeneration != 0 {
				t.Fatalf("stale generation published: %#v", latest.Status)
			}
			if change == "replace" && (latest.UID != "replacement" || latest.Status.Phase != "") {
				t.Fatalf("replacement mutated: %#v", latest)
			}
			if change == "terminal" && latest.Status.Message != "completed elsewhere" {
				t.Fatalf("external terminal status overwritten: %#v", latest.Status)
			}
		})
	}
}

func newIdentityTestReconciler(t *testing.T, kind machinav1alpha3.OperationKind, finish func(context.Context, client.Client, *machinav1alpha3.MachineOperation) error) (*machineOperationIdentityReconciler, client.Client, *fakeNodeOperator, *machinav1alpha3.MachineOperation, *machinav1alpha3.Machine) {
	t.Helper()
	machine := &machinav1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "worker", Generation: 7}}
	op := &machinav1alpha3.MachineOperation{ObjectMeta: metav1.ObjectMeta{Name: "op", UID: "original"}}
	op.Spec.OperationKind = kind
	op.Spec.MachineRef = machine.Name
	direct := fake.NewClientBuilder().WithScheme(newScheme()).WithRESTMapper(machineOperationRESTMapper()).
		WithObjects(machine, op).WithStatusSubresource(op).Build()
	writes := interceptor.NewClient(direct, interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			t.Fatal("operation reads must use the uncached reader, including conflict retries")
			return nil
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			operation, ok := obj.(*machinav1alpha3.MachineOperation)
			if !ok {
				t.Fatalf("unexpected status object %T", obj)
			}
			if operation.Status.IsTerminal() {
				if err := finish(ctx, c, operation); err != nil {
					return err
				}
			}
			return c.SubResource(subresource).Update(ctx, obj, opts...)
		},
	})
	operator := &fakeNodeOperator{}
	reconciler, err := machineOperationReconciler(machineOperationReconcilerOptions{
		Client: writes, Reader: direct, Log: slog.Default(), NodeName: machine.Name,
		AKSMachineName: machine.Name, Operator: operator, AgentUpgrade: &fakeAgentUpgradeExecutor{},
	})
	if err != nil {
		t.Fatal(err)
	}
	guarded, ok := reconciler.(*machineOperationIdentityReconciler)
	if !ok {
		t.Fatalf("unexpected reconciler %T", reconciler)
	}
	return guarded, direct, operator, op, machine
}
