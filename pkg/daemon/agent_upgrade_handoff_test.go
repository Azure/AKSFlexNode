package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/internal/agentdeployment"
	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type testUpgradeHandoff struct {
	signals      agentdeployment.UpgradeSignalStore
	instance     string
	validation   error
	active       bool
	activeErr    error
	lastGood     bool
	rollbackErr  error
	rollback     func()
	restarts     int
	publications []machinav1alpha3.OperationPhase
	client       client.Client
	operation    string
}

func (h *testUpgradeHandoff) SignalStore() agentdeployment.UpgradeSignalStore { return h.signals }
func (h *testUpgradeHandoff) DaemonInstance() string                          { return h.instance }
func (h *testUpgradeHandoff) ValidateStarted(*agentdeployment.UpgradeSignal) error {
	return h.validation
}
func (h *testUpgradeHandoff) CandidateActive(*agentdeployment.UpgradeSignal) (bool, error) {
	return h.active, h.activeErr
}
func (h *testUpgradeHandoff) RollbackSwitch(*agentdeployment.UpgradeSignal) error {
	if h.rollbackErr != nil {
		return h.rollbackErr
	}
	if h.rollback != nil {
		h.rollback()
	}
	return nil
}
func (h *testUpgradeHandoff) RunningLastGood(*agentdeployment.UpgradeSignal) (bool, error) {
	return h.lastGood, nil
}
func (h *testUpgradeHandoff) Restart(context.Context) error {
	h.restarts++
	return nil
}
func (h *testUpgradeHandoff) phases(t *testing.T) []machinav1alpha3.OperationPhase {
	t.Helper()

	var operation machinav1alpha3.MachineOperation
	if err := h.client.Get(t.Context(), client.ObjectKey{Name: h.operation}, &operation); err != nil {
		t.Fatal(err)
	}
	if len(h.publications) == 0 {
		if operation.Status.CompletedAt != nil {
			t.Fatalf("operation completed without a recorded publication: %#v", operation.Status)
		}
		return nil
	}
	if operation.Status.CompletedAt == nil || operation.Status.Phase != h.publications[len(h.publications)-1] {
		t.Fatalf("final status = %#v, publications = %v", operation.Status, h.publications)
	}
	return h.publications
}

type recordingStatusClient struct {
	client.Client
	publications *[]machinav1alpha3.OperationPhase
}

func (c recordingStatusClient) Status() client.SubResourceWriter {
	return recordingStatusWriter{SubResourceWriter: c.Client.Status(), publications: c.publications}
}

type recordingStatusWriter struct {
	client.SubResourceWriter
	publications *[]machinav1alpha3.OperationPhase
}

func (w recordingStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	operation, ok := obj.(*machinav1alpha3.MachineOperation)
	if !ok {
		return fmt.Errorf("unexpected status object %T", obj)
	}
	if err := w.SubResourceWriter.Update(ctx, obj, opts...); err != nil {
		return err
	}
	*w.publications = append(*w.publications, operation.Status.Phase)
	return nil
}

func newTestUpgradeHandoff(t *testing.T, signal agentdeployment.UpgradeSignal) *testUpgradeHandoff {
	t.Helper()

	h := &testUpgradeHandoff{
		signals:   agentdeployment.NewUpgradeSignalStore(filepath.Join(t.TempDir(), "signal.json")),
		instance:  "current-instance",
		operation: signal.OperationName,
	}
	baseClient := fake.NewClientBuilder().WithScheme(newScheme()).
		WithStatusSubresource(&machinav1alpha3.MachineOperation{}).
		WithObjects(&machinav1alpha3.MachineOperation{
			ObjectMeta: metav1.ObjectMeta{Name: signal.OperationName},
		}).Build()
	h.client = recordingStatusClient{Client: baseClient, publications: &h.publications}
	if err := h.signals.Write(signal); err != nil {
		t.Fatal(err)
	}
	return h
}

type retryUpgradeFixture struct {
	agentdeployment.UpgradeExecutor
	signals    agentdeployment.UpgradeSignalStore
	restarts   int
	restartErr error
}

func (f *retryUpgradeFixture) SignalStore() agentdeployment.UpgradeSignalStore { return f.signals }

func (f *retryUpgradeFixture) Restart(context.Context) error {
	f.restarts++
	return f.restartErr
}

func TestRetryAgentUpgradeRecoveryReportsScheduledRestart(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		signal     *agentdeployment.UpgradeSignal
		restartErr error
		want       bool
		wantErr    bool
		calls      int
	}{
		"no signal": {},
		"ordinary pending upgrade": {
			signal: &agentdeployment.UpgradeSignal{
				OperationName: "upgrade",
			},
		},
		"recovery scheduled": {
			signal: &agentdeployment.UpgradeSignal{
				OperationName:    "upgrade",
				RecoveryRequired: true,
			},
			want:  true,
			calls: 1,
		},
		"restart failed": {
			signal: &agentdeployment.UpgradeSignal{
				OperationName:    "upgrade",
				RecoveryRequired: true,
			},
			restartErr: errors.New("restart failed"),
			wantErr:    true,
			calls:      1,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			signals := agentdeployment.NewUpgradeSignalStore(filepath.Join(t.TempDir(), "signal.json"))
			if tc.signal != nil {
				if err := signals.Write(*tc.signal); err != nil {
					t.Fatal(err)
				}
			}
			fixture := &retryUpgradeFixture{
				signals:    signals,
				restartErr: tc.restartErr,
			}
			coordinator := &agentUpgradeCoordinator{UpgradeExecutor: fixture}

			got, err := coordinator.RetryRecovery(t.Context())
			if got != tc.want {
				t.Fatalf("scheduled = %v, want %v", got, tc.want)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error %v", err, tc.wantErr)
			}
			if fixture.restarts != tc.calls {
				t.Fatalf("restarts = %d, want %d", fixture.restarts, tc.calls)
			}
		})
	}
}

func TestPublishAgentUpgradeSignalPreservesSignalWhenRollbackFails(t *testing.T) {
	t.Parallel()

	h := newTestUpgradeHandoff(t, agentdeployment.UpgradeSignal{
		OperationName:            "operation-1",
		InitiatingDaemonInstance: "other-instance",
	})
	h.validation = errors.New("candidate failed")
	h.active = true
	h.rollbackErr = errors.New("rollback failed")
	if err := publishAndClearAgentUpgradeSignal(t.Context(), slog.Default(), h.client, h); err == nil {
		t.Fatal("publish succeeded after rollback failed")
	}
	if signal, err := h.signals.Read(); err != nil || signal == nil {
		t.Fatalf("signal after failed rollback = %#v, %v", signal, err)
	}
	if len(h.phases(t)) != 0 {
		t.Fatalf("published before rollback succeeded: %v", h.phases(t))
	}
}

func TestPublishAgentUpgradeFailureRestartsIntoLastGoodBeforeClearingSignal(t *testing.T) {
	t.Parallel()

	h := newTestUpgradeHandoff(t, agentdeployment.UpgradeSignal{
		OperationName:            "operation-1",
		InitiatingDaemonInstance: "initiator",
		RecoveryRequired:         true,
		Failure:                  "candidate failed",
	})
	rolledBack := false
	h.rollback = func() { rolledBack = true }
	if err := publishAndClearAgentUpgradeSignal(t.Context(), slog.Default(), h.client, h); err != nil {
		t.Fatal(err)
	}
	if !rolledBack || h.restarts != 1 || len(h.phases(t)) != 0 {
		t.Fatalf("rollback=%v restarts=%d finished=%v", rolledBack, h.restarts, h.phases(t))
	}
	if signal, err := h.signals.Read(); err != nil || signal == nil {
		t.Fatalf("signal cleared before last-good startup: %#v, %v", signal, err)
	}

	h.instance = "last-good-instance"
	h.lastGood = true
	if err := publishAndClearAgentUpgradeSignal(t.Context(), slog.Default(), h.client, h); err != nil {
		t.Fatal(err)
	}
	if len(h.phases(t)) != 1 || h.phases(t)[0] != machinav1alpha3.OperationPhaseFailed {
		t.Fatalf("published phases = %v, want Failed", h.phases(t))
	}
	if h.restarts != 1 {
		t.Fatalf("restarts = %d, want no additional restart", h.restarts)
	}
	if signal, err := h.signals.Read(); err != nil || signal != nil {
		t.Fatalf("signal after last-good startup = %#v, %v", signal, err)
	}
}

func TestPublishAgentUpgradeSuccessCompletesAndClearsSignal(t *testing.T) {
	t.Parallel()

	h := newTestUpgradeHandoff(t, agentdeployment.UpgradeSignal{
		OperationName:            "operation-1",
		InitiatingDaemonInstance: "previous-instance",
		SwitchCommitted:          true,
	})
	if err := publishAndClearAgentUpgradeSignal(t.Context(), slog.Default(), h.client, h); err != nil {
		t.Fatal(err)
	}
	if len(h.phases(t)) != 1 || h.phases(t)[0] != machinav1alpha3.OperationPhaseComplete {
		t.Fatalf("published phases = %v, want Complete", h.phases(t))
	}
	if signal, err := h.signals.Read(); err != nil || signal != nil {
		t.Fatalf("signal after success = %#v, %v", signal, err)
	}
}

type failingStatusClient struct{ client.Client }

func (c failingStatusClient) Status() client.SubResourceWriter {
	return failingStatusWriter{SubResourceWriter: c.Client.Status()}
}

type failingStatusWriter struct{ client.SubResourceWriter }

func (failingStatusWriter) Update(context.Context, client.Object, ...client.SubResourceUpdateOption) error {
	return errors.New("status write failed")
}

func TestPublishAgentUpgradeKeepsSignalWhenStatusWriteFails(t *testing.T) {
	t.Parallel()

	h := newTestUpgradeHandoff(t, agentdeployment.UpgradeSignal{
		OperationName:            "operation-1",
		InitiatingDaemonInstance: "previous-instance",
	})
	err := publishAndClearAgentUpgradeSignal(t.Context(), slog.Default(), failingStatusClient{h.client}, h)
	if err == nil || !strings.Contains(err.Error(), "publish AgentUpgrade result: status write failed") {
		t.Fatalf("publish error = %v", err)
	}
	if signal, readErr := h.signals.Read(); readErr != nil || signal == nil {
		t.Fatalf("signal after failed publication = %#v, %v", signal, readErr)
	}
	if len(h.phases(t)) != 0 {
		t.Fatalf("recorded a successful publication after a failed status write: %v", h.phases(t))
	}
}

func TestPublishAgentUpgradeSignalIgnoresInitiatingProcess(t *testing.T) {
	t.Parallel()

	h := newTestUpgradeHandoff(t, agentdeployment.UpgradeSignal{
		OperationName:            "operation-1",
		InitiatingDaemonInstance: "current-instance",
	})
	if err := publishAndClearAgentUpgradeSignal(t.Context(), slog.Default(), h.client, h); err != nil {
		t.Fatal(err)
	}
	if signal, err := h.signals.Read(); err != nil || signal == nil {
		t.Fatalf("initiating process consumed signal: %#v, %v", signal, err)
	}
	if len(h.phases(t)) != 0 {
		t.Fatalf("initiating process published: %v", h.phases(t))
	}
}
