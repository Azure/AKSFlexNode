package drift

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"
	"github.com/sirupsen/logrus"

	"github.com/Azure/AKSFlexNode/pkg/bootstrapper"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/spec"
	"github.com/Azure/AKSFlexNode/pkg/status"
	"github.com/Azure/AKSFlexNode/pkg/systemd"
)

type countingDetector struct {
	called int32
	fn     func() ([]Finding, error)
}

func (d *countingDetector) Name() string { return "counting" }

func (d *countingDetector) Detect(ctx context.Context, _ *config.Config, _ *spec.ManagedClusterSpec, _ *status.NodeStatus) ([]Finding, error) {
	_ = ctx
	atomic.AddInt32(&d.called, 1)
	if d.fn == nil {
		return nil, nil
	}
	return d.fn()
}

func TestIsManagedClusterSpecStale(t *testing.T) {
	t.Parallel()

	if !isManagedClusterSpecStale(nil, time.Now()) {
		t.Fatalf("nil spec should be stale")
	}
	if !isManagedClusterSpecStale(&spec.ManagedClusterSpec{}, time.Now()) {
		t.Fatalf("zero CollectedAt should be stale")
	}
	if isManagedClusterSpecStale(&spec.ManagedClusterSpec{CollectedAt: time.Now()}, time.Now()) {
		t.Fatalf("fresh spec should not be stale")
	}
	old := time.Now().Add(-maxManagedClusterSpecAge - time.Minute)
	if !isManagedClusterSpecStale(&spec.ManagedClusterSpec{CollectedAt: old}, time.Now()) {
		t.Fatalf("old spec should be stale")
	}
}

func TestResolveRemediationPlan(t *testing.T) {
	t.Parallel()

	plan, requires, err := resolveRemediationPlan(nil)
	if err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
	if requires {
		t.Fatalf("requiresRemediation=true, want false")
	}
	if plan.Action != RemediationActionUnspecified {
		t.Fatalf("plan.Action=%q, want %q", plan.Action, RemediationActionUnspecified)
	}

	plan, requires, err = resolveRemediationPlan([]Finding{{
		ID:          "f1",
		Remediation: Remediation{Action: RemediationActionKubernetesUpgrade, KubernetesVersion: "1.30.7"},
	}})
	if err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
	if !requires {
		t.Fatalf("requiresRemediation=false, want true")
	}
	if plan.Action != RemediationActionKubernetesUpgrade {
		t.Fatalf("plan.Action=%q, want %q", plan.Action, RemediationActionKubernetesUpgrade)
	}
	if plan.DesiredKubernetesVersion != "1.30.7" {
		t.Fatalf("DesiredKubernetesVersion=%q, want %q", plan.DesiredKubernetesVersion, "1.30.7")
	}

	_, _, err = resolveRemediationPlan([]Finding{
		{ID: "a", Remediation: Remediation{Action: RemediationActionKubernetesUpgrade, KubernetesVersion: "1.30.7"}},
		{ID: "b", Remediation: Remediation{Action: RemediationActionKubernetesUpgrade, KubernetesVersion: "1.31.0"}},
	})
	if err == nil {
		t.Fatalf("err=nil, want conflict error")
	}

	_, _, err = resolveRemediationPlan([]Finding{
		{ID: "a", Remediation: Remediation{Action: RemediationActionKubernetesUpgrade}},
		{ID: "b", Remediation: Remediation{Action: RemediationActionUnspecified}},
	})
	if err != nil {
		t.Fatalf("err=%v, want nil", err)
	}

	_, _, err = resolveRemediationPlan([]Finding{
		{ID: "a", Remediation: Remediation{Action: RemediationActionKubernetesUpgrade}},
		{ID: "b", Remediation: Remediation{Action: "something-else"}},
	})
	if err == nil {
		t.Fatalf("err=nil, want action conflict error")
	}
}

func TestDetectAndRemediate_SkipsStaleSpec_DoesNotCallDetectors(t *testing.T) {
	t.Parallel()

	logger := logrus.New()
	d := &countingDetector{fn: func() ([]Finding, error) {
		return []Finding{{
			ID:          "f1",
			Remediation: Remediation{Action: RemediationActionKubernetesUpgrade, KubernetesVersion: "1.30.0"},
		}}, nil
	}}

	staleCollectedAt := time.Now().Add(-maxManagedClusterSpecAge - time.Minute)
	specSnap := &spec.ManagedClusterSpec{CurrentKubernetesVersion: "1.30.0", CollectedAt: staleCollectedAt}
	statusSnap := &status.NodeStatus{KubeletVersion: "1.29.0"}

	err := detectAndRemediate(context.Background(), nil, logger, nil, []Detector{d}, specSnap, statusSnap, nil)
	if err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
	if got := atomic.LoadInt32(&d.called); got != 0 {
		t.Fatalf("detector called %d times, want 0", got)
	}
}

func TestDetectAndRemediate_BootstrapGuard_SkipsWhenInProgress(t *testing.T) {
	t.Parallel()

	logger := logrus.New()
	d := &countingDetector{fn: func() ([]Finding, error) {
		return []Finding{{
			ID:          "f1",
			Remediation: Remediation{Action: RemediationActionKubernetesUpgrade, KubernetesVersion: "1.31.0"},
		}}, nil
	}}

	specSnap := &spec.ManagedClusterSpec{CurrentKubernetesVersion: "1.31.0", CollectedAt: time.Now()}
	statusSnap := &status.NodeStatus{KubeletVersion: "1.30.0"}

	var bootstrapInProgress int32 = 1
	err := detectAndRemediate(context.Background(), nil, logger, &bootstrapInProgress, []Detector{d}, specSnap, statusSnap, nil)
	if err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
	if got := atomic.LoadInt32(&d.called); got != 1 {
		t.Fatalf("detector called %d times, want 1", got)
	}
	if got := atomic.LoadInt32(&bootstrapInProgress); got != 1 {
		t.Fatalf("bootstrapInProgress=%d, want 1", got)
	}
}

func TestDetectAndRemediate_ReturnsDetectErrorIfNoFindings(t *testing.T) {
	t.Parallel()

	logger := logrus.New()
	wantErr := errors.New("detect failed")
	d := &countingDetector{fn: func() ([]Finding, error) {
		return nil, wantErr
	}}

	specSnap := &spec.ManagedClusterSpec{CurrentKubernetesVersion: "1.31.0", CollectedAt: time.Now()}
	statusSnap := &status.NodeStatus{KubeletVersion: "1.30.0"}

	err := detectAndRemediate(context.Background(), nil, logger, nil, []Detector{d}, specSnap, statusSnap, nil)
	if err == nil {
		t.Fatalf("err=nil, want %v", wantErr)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("err=%v, want to contain %v", err, wantErr)
	}
}

func TestShouldMarkKubeletUnhealthyAfterUpgradeFailure(t *testing.T) {
	t.Parallel()

	makeResultFailingAt := func(step string) *bootstrapper.ExecutionResult {
		return &bootstrapper.ExecutionResult{
			StepResults: []bootstrapper.StepResult{
				{StepName: step, Success: false, Error: "boom"},
			},
			Error: fmt.Sprintf("failed at %s", step),
		}
	}

	err := errors.New("boom")

	if got := shouldMarkKubeletUnhealthyAfterUpgradeFailure(makeResultFailingAt(upgradeStepCordonAndDrain), err); got {
		t.Fatalf("cordon-and-drain failure marked unhealthy=true, want false")
	}
	if got := shouldMarkKubeletUnhealthyAfterUpgradeFailure(makeResultFailingAt(upgradeStepUncordon), err); got {
		t.Fatalf("uncordon failure marked unhealthy=true, want false")
	}

	if got := shouldMarkKubeletUnhealthyAfterUpgradeFailure(makeResultFailingAt(upgradeStepStopKubelet), err); !got {
		t.Fatalf("stop-kubelet failure marked unhealthy=false, want true")
	}
	if got := shouldMarkKubeletUnhealthyAfterUpgradeFailure(makeResultFailingAt(upgradeStepDownloadKubeBinaries), err); !got {
		t.Fatalf("download-kube-binaries failure marked unhealthy=false, want true")
	}
	if got := shouldMarkKubeletUnhealthyAfterUpgradeFailure(makeResultFailingAt(upgradeStepStartKubelet), err); !got {
		t.Fatalf("start-kubelet failure marked unhealthy=false, want true")
	}

	// Unknown step -> dont mark kubelet unhealthy
	if got := shouldMarkKubeletUnhealthyAfterUpgradeFailure(makeResultFailingAt("something-else"), err); got {
		t.Fatalf("unknown step marked unhealthy=true, want false")
	}
	// No error -> never mark.
	if got := shouldMarkKubeletUnhealthyAfterUpgradeFailure(makeResultFailingAt(upgradeStepStopKubelet), nil); got {
		t.Fatalf("nil error marked unhealthy=true, want false")
	}
}

// stubSystemdManager is a test double for systemd.Manager.
type stubSystemdManager struct {
	getUnitStatusResult dbus.UnitStatus
	getUnitStatusErr    error
}

var _ systemd.Manager = (*stubSystemdManager)(nil)

func (s *stubSystemdManager) DaemonReload(_ context.Context) error                  { return nil }
func (s *stubSystemdManager) EnableUnit(_ context.Context, _ string) error          { return nil }
func (s *stubSystemdManager) DisableUnit(_ context.Context, _ string) error         { return nil }
func (s *stubSystemdManager) MaskUnit(_ context.Context, _ string) error            { return nil }
func (s *stubSystemdManager) StartUnit(_ context.Context, _ string) error           { return nil }
func (s *stubSystemdManager) StopUnit(_ context.Context, _ string) error            { return nil }
func (s *stubSystemdManager) ReloadOrRestartUnit(_ context.Context, _ string) error { return nil }

func (s *stubSystemdManager) GetUnitStatus(_ context.Context, _ string) (dbus.UnitStatus, error) {
	return s.getUnitStatusResult, s.getUnitStatusErr
}

func (s *stubSystemdManager) EnsureUnitFile(_ context.Context, _ string, _ []byte) (bool, error) {
	return false, nil
}

func (s *stubSystemdManager) EnsureDropInFile(_ context.Context, _ string, _ string, _ []byte) (bool, error) {
	return false, nil
}

// stubRebootRunner records whether it was called and returns a configurable result.
type stubRebootRunner struct {
	called bool
	output []byte
	err    error
}

func (s *stubRebootRunner) run(_ context.Context) ([]byte, error) {
	s.called = true
	return s.output, s.err
}

func TestRunRebootRemediationWithDeps(t *testing.T) {
	t.Parallel()

	statusActive := dbus.UnitStatus{ActiveState: systemd.UnitActiveStateActive}
	statusInactive := dbus.UnitStatus{ActiveState: systemd.UnitActiveStateInactive}
	dbusErr := errors.New("dbus connection refused")
	rebootErr := errors.New("exit status 1")
	timeoutErr := errors.New("signal: killed")

	// pastDeadlineCtx returns a context whose deadline has already elapsed so that
	// runRebootRemediationWithDeps receives a context with hasDeadline==true and
	// ctx.Err()==DeadlineExceeded, exercising the timeout error path.
	pastDeadlineCtx := func() (context.Context, context.CancelFunc) {
		return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	}

	tests := []struct {
		name             string
		mgr              *stubSystemdManager
		runner           *stubRebootRunner
		makeCtx          func() (context.Context, context.CancelFunc)
		wantErr          error
		wantErrWrapped   bool // whether wantErr should be wrapped inside the returned error
		wantRunnerCalled bool
	}{
		{
			name:             "unit-not-found/no-op",
			mgr:              &stubSystemdManager{getUnitStatusErr: systemd.ErrUnitNotFound},
			runner:           &stubRebootRunner{},
			wantRunnerCalled: false,
		},
		{
			name:             "inactive/no-op",
			mgr:              &stubSystemdManager{getUnitStatusResult: statusInactive},
			runner:           &stubRebootRunner{},
			wantRunnerCalled: false,
		},
		{
			name:             "active/executes-reboot",
			mgr:              &stubSystemdManager{getUnitStatusResult: statusActive},
			runner:           &stubRebootRunner{},
			wantRunnerCalled: true,
		},
		{
			name:             "get-unit-status-error/surfaced",
			mgr:              &stubSystemdManager{getUnitStatusErr: dbusErr},
			runner:           &stubRebootRunner{},
			wantErr:          dbusErr,
			wantErrWrapped:   true,
			wantRunnerCalled: false,
		},
		{
			name:             "reboot-command-fails/surfaced",
			mgr:              &stubSystemdManager{getUnitStatusResult: statusActive},
			runner:           &stubRebootRunner{err: rebootErr},
			wantErr:          rebootErr,
			wantErrWrapped:   true,
			wantRunnerCalled: true,
		},
		{
			name:             "reboot-command-times-out/surfaced",
			mgr:              &stubSystemdManager{getUnitStatusResult: statusActive},
			runner:           &stubRebootRunner{err: timeoutErr},
			makeCtx:          pastDeadlineCtx,
			wantErr:          timeoutErr,
			wantErrWrapped:   true,
			wantRunnerCalled: true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var ctx context.Context
			var cancel context.CancelFunc
			if tc.makeCtx != nil {
				ctx, cancel = tc.makeCtx()
			} else {
				ctx, cancel = context.WithCancel(context.Background())
			}
			defer cancel()

			err := runRebootRemediationWithDeps(ctx, logrus.New(), tc.mgr, tc.runner.run)

			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("err=%v, want nil", err)
				}
			} else {
				if err == nil {
					t.Fatalf("err=nil, want error wrapping %v", tc.wantErr)
				}
				if tc.wantErrWrapped && !errors.Is(err, tc.wantErr) {
					t.Fatalf("err=%v, want to wrap %v", err, tc.wantErr)
				}
			}

			if tc.wantRunnerCalled != tc.runner.called {
				t.Fatalf("runner.called=%v, want %v", tc.runner.called, tc.wantRunnerCalled)
			}
		})
	}
}
