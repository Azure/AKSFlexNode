package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/agent/agentbinary"
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
		Operator:             &fakeNodeOperator{},
		AgentUpgrade:         &fakeAgentUpgradeExecutor{},
	})
	if err != nil {
		t.Fatalf("machineOperationReconciler: %v", err)
	}
	if reconciler == nil {
		t.Fatal("machineOperationReconciler returned nil")
	}
}

func TestMachineOperationReconcilerRequiresDependencies(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		opts    machineOperationReconcilerOptions
		wantErr string
	}{
		"missing logger": {
			opts: machineOperationReconcilerOptions{
				Client:   fake.NewClientBuilder().Build(),
				Operator: &fakeNodeOperator{},
			},
			wantErr: "logger is nil",
		},
		"missing client": {
			opts: machineOperationReconcilerOptions{
				Log:      slog.Default(),
				Operator: &fakeNodeOperator{},
			},
			wantErr: "kubernetes client is nil",
		},
		"missing operator": {
			opts: machineOperationReconcilerOptions{
				Client:       fake.NewClientBuilder().Build(),
				Log:          slog.Default(),
				AgentUpgrade: &fakeAgentUpgradeExecutor{},
			},
			wantErr: "node operator is nil",
		},
		"missing agent upgrade executor": {
			opts: machineOperationReconcilerOptions{
				Client:   fake.NewClientBuilder().Build(),
				Log:      slog.Default(),
				Operator: &fakeNodeOperator{},
			},
			wantErr: "agent upgrade executor is nil",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := machineOperationReconciler(tt.opts)
			if err == nil {
				t.Fatal("machineOperationReconciler error = nil, want error")
			}
			if err.Error() != tt.wantErr {
				t.Fatalf("error = %q, want %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestMachineOperationReconcilerEnabledRequiresNames(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		nodeName       string
		aksMachineName string
		wantErr        string
	}{
		"missing node name": {
			aksMachineName: "machine1",
			wantErr:        "node name is empty",
		},
		"missing AKS machine name": {
			nodeName: "node1",
			wantErr:  "AKS machine name is empty",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := machineOperationReconciler(machineOperationReconcilerOptions{
				Client: fake.NewClientBuilder().WithRESTMapper(
					machineOperationRESTMapper(),
				).Build(),
				Log:            slog.Default(),
				NodeName:       tt.nodeName,
				AKSMachineName: tt.aksMachineName,
				Operator:       &fakeNodeOperator{},
				AgentUpgrade:   &fakeAgentUpgradeExecutor{},
			})
			if err == nil {
				t.Fatal("machineOperationReconciler error = nil, want error")
			}
			if err.Error() != tt.wantErr {
				t.Fatalf("error = %q, want %q", err.Error(), tt.wantErr)
			}
		})
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

func TestMachineOperationHandlersNodeRebootFailure(t *testing.T) {
	t.Parallel()

	operator := &fakeNodeOperator{restartErr: errors.New("restart failed")}
	store := &fakeMachineOperationStore{}
	target := &machineOperationHandlers{log: slog.Default(), operator: operator}

	if _, err := target.reconcileNodeReboot(t.Context(), store, daemon.MachineOperation{Name: "op1", Kind: machinav1alpha3.OperationNodeReboot}); err != nil {
		t.Fatalf("reconcileNodeReboot: %v", err)
	}
	if store.result.Phase != machinav1alpha3.OperationPhaseFailed {
		t.Fatalf("phase = %s, want %s", store.result.Phase, machinav1alpha3.OperationPhaseFailed)
	}
	if store.result.Reason != "ExecutionFailed" {
		t.Fatalf("reason = %s, want ExecutionFailed", store.result.Reason)
	}
	if store.result.Message != "restart failed" {
		t.Fatalf("message = %q, want restart failed", store.result.Message)
	}
}

func TestMachineOperationHandlersNodeRebootStoreFailure(t *testing.T) {
	t.Parallel()

	store := &fakeMachineOperationStore{markErr: errors.New("mark failed")}
	target := &machineOperationHandlers{log: slog.Default(), operator: &fakeNodeOperator{}}

	_, err := target.reconcileNodeReboot(t.Context(), store, daemon.MachineOperation{Name: "op1", Kind: machinav1alpha3.OperationNodeReboot})
	if err == nil {
		t.Fatal("reconcileNodeReboot error = nil, want error")
	}
	if !strings.Contains(err.Error(), "mark NodeReboot MachineOperation in progress") {
		t.Fatalf("error = %q, want mark context", err.Error())
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

func TestMachineOperationHandlersAgentUpgrade(t *testing.T) {
	t.Parallel()

	upgrader := &fakeAgentUpgradeExecutor{}
	store := &fakeMachineOperationStore{}
	target := &machineOperationHandlers{log: slog.Default(), operator: &fakeNodeOperator{}, agentUpgrade: upgrader}
	digest := strings.Repeat("a", 64)
	op := daemon.MachineOperation{
		Name: "upgrade-1",
		Kind: machinav1alpha3.OperationAgentUpgrade,
		Parameters: map[string]string{
			agentUpgradeDownloadURLParameter: "https://example.com/agent.tar.gz?sig=secret",
			agentUpgradeSHA256Parameter:      digest,
		},
	}

	if _, err := target.reconcileAgentUpgrade(t.Context(), store, op); err != nil {
		t.Fatalf("reconcileAgentUpgrade: %v", err)
	}
	if !store.inProgress || !upgrader.pending || !upgrader.staged || !upgrader.restarted {
		t.Fatalf("upgrade calls = inProgress:%v pending:%v staged:%v restarted:%v", store.inProgress, upgrader.pending, upgrader.staged, upgrader.restarted)
	}
	if store.result.Phase != "" {
		t.Fatalf("phase = %s, want non-terminal until daemon restart", store.result.Phase)
	}
}

func TestMachineOperationHandlersAgentUpgradeInvalidParameters(t *testing.T) {
	t.Parallel()

	upgrader := &fakeAgentUpgradeExecutor{}
	store := &fakeMachineOperationStore{}
	target := &machineOperationHandlers{log: slog.Default(), operator: &fakeNodeOperator{}, agentUpgrade: upgrader}

	if _, err := target.reconcileAgentUpgrade(t.Context(), store, daemon.MachineOperation{Name: "upgrade-1", Kind: machinav1alpha3.OperationAgentUpgrade}); err != nil {
		t.Fatalf("reconcileAgentUpgrade: %v", err)
	}
	if store.result.Phase != machinav1alpha3.OperationPhaseFailed || store.result.Reason != "InvalidParameters" {
		t.Fatalf("result = %#v, want InvalidParameters failure", store.result)
	}
	if upgrader.pending || upgrader.staged || upgrader.restarted {
		t.Fatal("executor called for invalid parameters")
	}
}

func TestMachineOperationHandlersAgentUpgradeDuplicateReconcileIsNoop(t *testing.T) {
	t.Parallel()

	upgrader := &fakeAgentUpgradeExecutor{pendingErr: errAgentUpgradeAlreadyPending}
	store := &fakeMachineOperationStore{}
	target := &machineOperationHandlers{log: slog.Default(), operator: &fakeNodeOperator{}, agentUpgrade: upgrader}
	op := daemon.MachineOperation{Name: "upgrade-1", Parameters: map[string]string{
		agentUpgradeDownloadURLParameter: "https://example.com/agent.tar.gz",
		agentUpgradeSHA256Parameter:      strings.Repeat("a", 64),
	}}

	if _, err := target.reconcileAgentUpgrade(t.Context(), store, op); err != nil {
		t.Fatalf("reconcileAgentUpgrade: %v", err)
	}
	if upgrader.staged || upgrader.restarted || upgrader.aborted {
		t.Fatal("duplicate reconciliation executed upgrade actions")
	}
	if store.result.Phase != "" {
		t.Fatalf("duplicate reconciliation produced terminal phase %s", store.result.Phase)
	}
}

func TestMachineOperationHandlersAgentUpgradeRequeuesWhenDirectActivationHoldsLock(t *testing.T) {
	t.Parallel()

	upgrader := &fakeAgentUpgradeExecutor{acquireErr: agentbinary.ErrActivationInProgress}
	store := &fakeMachineOperationStore{}
	target := &machineOperationHandlers{log: slog.Default(), operator: &fakeNodeOperator{}, agentUpgrade: upgrader}
	op := daemon.MachineOperation{Name: "upgrade-1", Parameters: map[string]string{
		agentUpgradeDownloadURLParameter: "https://example.com/agent.tar.gz",
		agentUpgradeSHA256Parameter:      strings.Repeat("a", 64),
	}}
	result, err := target.reconcileAgentUpgrade(t.Context(), store, op)
	if err != nil {
		t.Fatalf("reconcileAgentUpgrade: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("RequeueAfter = %v, want positive duration", result.RequeueAfter)
	}
	if store.inProgress || upgrader.pending || upgrader.staged || upgrader.restarted {
		t.Fatal("lock contention mutated the operation or staged an upgrade")
	}
}

func TestMachineOperationHandlersAgentUpgradeStageFailureRollsBack(t *testing.T) {
	t.Parallel()

	upgrader := &fakeAgentUpgradeExecutor{stageErr: errors.New("bad archive")}
	store := &fakeMachineOperationStore{}
	target := &machineOperationHandlers{log: slog.Default(), operator: &fakeNodeOperator{}, agentUpgrade: upgrader}
	op := daemon.MachineOperation{Name: "upgrade-1", Parameters: map[string]string{
		agentUpgradeDownloadURLParameter: "https://example.com/agent.tar.gz",
		agentUpgradeSHA256Parameter:      strings.Repeat("a", 64),
	}}

	if _, err := target.reconcileAgentUpgrade(t.Context(), store, op); err != nil {
		t.Fatalf("reconcileAgentUpgrade: %v", err)
	}
	if !upgrader.aborted {
		t.Fatal("Abort was not called")
	}
	if store.result.Phase != machinav1alpha3.OperationPhaseFailed || store.result.Message != "bad archive" {
		t.Fatalf("result = %#v", store.result)
	}
}

func TestMachineOperationHandlersAgentUpgradeAbortFailureStartsRecovery(t *testing.T) {
	t.Parallel()

	upgrader := &fakeAgentUpgradeExecutor{
		stageErr: errors.New("stage failed"),
		abortErr: errors.New("rollback failed"),
	}
	store := &fakeMachineOperationStore{}
	target := &machineOperationHandlers{log: slog.Default(), operator: &fakeNodeOperator{}, agentUpgrade: upgrader}
	op := daemon.MachineOperation{Name: "upgrade-1", Parameters: map[string]string{
		agentUpgradeDownloadURLParameter: "https://example.com/agent.tar.gz",
		agentUpgradeSHA256Parameter:      strings.Repeat("a", 64),
	}}

	if _, err := target.reconcileAgentUpgrade(t.Context(), store, op); err != nil {
		t.Fatalf("reconcileAgentUpgrade: %v", err)
	}
	if upgrader.failure == "" || !upgrader.restarted {
		t.Fatalf("recovery failure = %q, restarted = %v", upgrader.failure, upgrader.restarted)
	}
	if store.result.Phase != "" {
		t.Fatalf("phase = %s, want recovery to publish terminal status", store.result.Phase)
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
		t.Fatal("ResetNode was not called")
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

func TestMachineOperationHandlersAgentResetFailure(t *testing.T) {
	t.Parallel()

	operator := &fakeNodeOperator{resetErr: errors.New("reset failed")}
	store := &fakeMachineOperationStore{}
	target := &machineOperationHandlers{log: slog.Default(), operator: operator}

	if _, err := target.reconcileAgentReset(t.Context(), store, daemon.MachineOperation{Name: "op1", Kind: machinav1alpha3.OperationAgentReset}); err != nil {
		t.Fatalf("reconcileAgentReset: %v", err)
	}
	if store.result.Phase != machinav1alpha3.OperationPhaseFailed {
		t.Fatalf("phase = %s, want %s", store.result.Phase, machinav1alpha3.OperationPhaseFailed)
	}
	if store.result.Reason != "ExecutionFailed" {
		t.Fatalf("reason = %s, want ExecutionFailed", store.result.Reason)
	}
}

func TestMachineOperationHandlersAgentResetStopFailure(t *testing.T) {
	t.Parallel()

	operator := &fakeNodeOperator{stopErr: errors.New("stop failed")}
	store := &fakeMachineOperationStore{}
	target := &machineOperationHandlers{log: slog.Default(), operator: operator}

	_, err := target.reconcileAgentReset(t.Context(), store, daemon.MachineOperation{Name: "op1", Kind: machinav1alpha3.OperationAgentReset})
	if err == nil {
		t.Fatal("reconcileAgentReset error = nil, want error")
	}
	if !strings.Contains(err.Error(), "stop daemon after AgentReset MachineOperation") {
		t.Fatalf("error = %q, want stop context", err.Error())
	}
	if store.result.Phase != machinav1alpha3.OperationPhaseComplete {
		t.Fatalf("phase = %s, want %s", store.result.Phase, machinav1alpha3.OperationPhaseComplete)
	}
}

type fakeAgentUpgradeExecutor struct {
	pending    bool
	staged     bool
	aborted    bool
	restarted  bool
	failure    string
	acquireErr error
	pendingErr error
	stageErr   error
	abortErr   error
	restartErr error
}

func (f *fakeAgentUpgradeExecutor) Acquire() (io.Closer, error) {
	if f.acquireErr != nil {
		return nil, f.acquireErr
	}
	return io.NopCloser(strings.NewReader("")), nil
}

func (f *fakeAgentUpgradeExecutor) RecordPending(context.Context, string) error {
	f.pending = true
	return f.pendingErr
}

func (f *fakeAgentUpgradeExecutor) RecordFailure(message string) error {
	f.failure = message
	return nil
}

func (f *fakeAgentUpgradeExecutor) Stage(context.Context, agentUpgradeRequest) error {
	f.staged = true
	return f.stageErr
}

func (f *fakeAgentUpgradeExecutor) Abort(context.Context) error {
	f.aborted = true
	return f.abortErr
}

func (f *fakeAgentUpgradeExecutor) Restart(context.Context) error {
	f.restarted = true
	return f.restartErr
}

var _ agentUpgradeExecutor = (*fakeAgentUpgradeExecutor)(nil)

type fakeMachineOperationStore struct {
	inProgress bool
	operation  daemon.MachineOperation
	result     daemon.MachineOperationResult[int64]
	markErr    error
	finishErr  error
}

func (f *fakeMachineOperationStore) MarkInProgress(_ context.Context, op daemon.MachineOperation, _ string) error {
	f.inProgress = true
	f.operation = op
	return f.markErr
}

func (f *fakeMachineOperationStore) Finish(_ context.Context, op daemon.MachineOperation, result daemon.MachineOperationResult[int64]) error {
	f.operation = op
	f.result = result
	return f.finishErr
}

var _ daemon.MachineOperationStore[int64] = (*fakeMachineOperationStore)(nil)
