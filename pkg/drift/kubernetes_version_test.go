package drift

import (
	"context"
	"testing"
	"time"

	"go.goms.io/aks/AKSFlexNode/pkg/spec"
	"go.goms.io/aks/AKSFlexNode/pkg/status"
)

func TestKubernetesVersionDetector_Detect_RespectsContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	d := NewKubernetesVersionDetector()
	_, err := d.Detect(ctx, nil, &spec.ManagedClusterSpec{KubernetesVersion: "1.30.0", CollectedAt: time.Now()}, &status.NodeStatus{KubeletVersion: "1.29.0"})
	if err == nil {
		t.Fatalf("err=nil, want context cancellation error")
	}
}

func TestKubernetesVersionDetector_Detect_NoDesiredVersion_NoFinding(t *testing.T) {
	t.Parallel()

	d := NewKubernetesVersionDetector()
	findings, err := d.Detect(context.Background(), nil, &spec.ManagedClusterSpec{CollectedAt: time.Now()}, &status.NodeStatus{KubeletVersion: "1.29.0"})
	if err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
	if len(findings) != 0 {
		t.Fatalf("findings len=%d, want 0", len(findings))
	}
}

func TestKubernetesVersionDetector_Detect_UnknownCurrent_NoFinding(t *testing.T) {
	t.Parallel()

	d := NewKubernetesVersionDetector()
	findings, err := d.Detect(context.Background(), nil, &spec.ManagedClusterSpec{KubernetesVersion: "1.30.0", CollectedAt: time.Now()}, &status.NodeStatus{KubeletVersion: "unknown"})
	if err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
	if len(findings) != 0 {
		t.Fatalf("findings len=%d, want 0", len(findings))
	}
}

func TestKubernetesVersionDetector_Detect_UpgradeOnly_Finding(t *testing.T) {
	t.Parallel()

	d := NewKubernetesVersionDetector()
	specSnap := &spec.ManagedClusterSpec{CurrentKubernetesVersion: "1.30.7", CollectedAt: time.Now()}
	statusSnap := &status.NodeStatus{KubeletVersion: "1.29.8"}

	findings, err := d.Detect(context.Background(), nil, specSnap, statusSnap)
	if err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings len=%d, want 1", len(findings))
	}
	if findings[0].ID != KubernetesVersionFindingID {
		t.Fatalf("finding ID=%q, want %q", findings[0].ID, KubernetesVersionFindingID)
	}
	if findings[0].Remediation.Action != RemediationActionKubernetesUpgrade {
		t.Fatalf("action=%q, want %q", findings[0].Remediation.Action, RemediationActionKubernetesUpgrade)
	}
	if findings[0].Remediation.KubernetesVersion != "1.30.7" {
		t.Fatalf("kubernetesVersion=%q, want %q", findings[0].Remediation.KubernetesVersion, "1.30.7")
	}
}

func TestKubernetesVersionDetector_Detect_NoDowngrade_NoFinding(t *testing.T) {
	t.Parallel()

	d := NewKubernetesVersionDetector()
	specSnap := &spec.ManagedClusterSpec{KubernetesVersion: "1.29.0", CollectedAt: time.Now()}
	statusSnap := &status.NodeStatus{KubeletVersion: "1.30.1"}

	findings, err := d.Detect(context.Background(), nil, specSnap, statusSnap)
	if err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
	if len(findings) != 0 {
		t.Fatalf("findings len=%d, want 0", len(findings))
	}
}

func TestKubernetesVersionDetector_Detect_SameMajorMinor_NoFinding(t *testing.T) {
	t.Parallel()

	d := NewKubernetesVersionDetector()
	specSnap := &spec.ManagedClusterSpec{CurrentKubernetesVersion: "1.30.7", CollectedAt: time.Now()}
	statusSnap := &status.NodeStatus{KubeletVersion: "1.30.1"}

	findings, err := d.Detect(context.Background(), nil, specSnap, statusSnap)
	if err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
	if len(findings) != 0 {
		t.Fatalf("findings len=%d, want 0", len(findings))
	}
}

func TestKubernetesVersionDetector_Detect_UnparseableVersions_NoFinding(t *testing.T) {
	t.Parallel()

	d := NewKubernetesVersionDetector()
	specSnap := &spec.ManagedClusterSpec{CurrentKubernetesVersion: "1.30.7", CollectedAt: time.Now()}
	statusSnap := &status.NodeStatus{KubeletVersion: "v1.x"}

	findings, err := d.Detect(context.Background(), nil, specSnap, statusSnap)
	if err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
	if len(findings) != 0 {
		t.Fatalf("findings len=%d, want 0", len(findings))
	}
}
