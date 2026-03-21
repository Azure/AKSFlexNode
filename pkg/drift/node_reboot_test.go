package drift

import (
	"context"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/status"
)

func TestRebootDetector_Name(t *testing.T) {
	t.Parallel()
	d := NewRebootDetector()
	if name := d.Name(); name != "RebootDetector" {
		t.Errorf("expected name %q, got %q", "RebootDetector", name)
	}
}

func TestRebootDetector_NilStatus_NoFindings(t *testing.T) {
	t.Parallel()
	d := NewRebootDetector()
	findings, err := d.Detect(context.Background(), nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("expected no findings, got %d", len(findings))
	}
}

func TestRebootDetector_NeedRebootFalse_NoFindings(t *testing.T) {
	t.Parallel()
	d := NewRebootDetector()
	statusSnap := &status.NodeStatus{
		NeedReboot: false,
	}
	findings, err := d.Detect(context.Background(), nil, nil, statusSnap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("expected no findings, got %d", len(findings))
	}
}

func TestRebootDetector_NeedRebootTrue_ReturnsFinding(t *testing.T) {
	t.Parallel()
	d := NewRebootDetector()
	statusSnap := &status.NodeStatus{
		NeedReboot: true,
	}
	findings, err := d.Detect(context.Background(), nil, nil, statusSnap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}

	f := findings[0]
	if f.ID != NodeRebootFindingID {
		t.Errorf("expected ID %q, got %q", NodeRebootFindingID, f.ID)
	}
	if f.Title != "Node reboot required" {
		t.Errorf("unexpected title: %q", f.Title)
	}
	if f.Remediation.Action != RemediationActionReboot {
		t.Errorf("expected action %q, got %q", RemediationActionReboot, f.Remediation.Action)
	}
}

func TestRebootDetector_CanceledContext_ReturnsError(t *testing.T) {
	t.Parallel()
	d := NewRebootDetector()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	statusSnap := &status.NodeStatus{NeedReboot: true}
	_, err := d.Detect(ctx, nil, nil, statusSnap)
	if err == nil {
		t.Fatal("expected error from canceled context")
	}
}
