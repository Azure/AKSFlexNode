package drift

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/Azure/AKSFlexNode/pkg/bootstrapper"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/spec"
	"github.com/Azure/AKSFlexNode/pkg/status"
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
