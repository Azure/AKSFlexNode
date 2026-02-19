package drift

import (
	"context"
	"errors"
	"testing"

	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/spec"
	"go.goms.io/aks/AKSFlexNode/pkg/status"
)

type detectorFunc struct {
	name string
	fn   func(ctx context.Context, cfg *config.Config, specSnap *spec.ManagedClusterSpec, statusSnap *status.NodeStatus) ([]Finding, error)
}

func (d detectorFunc) Name() string { return d.name }

func (d detectorFunc) Detect(ctx context.Context, cfg *config.Config, specSnap *spec.ManagedClusterSpec, statusSnap *status.NodeStatus) ([]Finding, error) {
	if d.fn == nil {
		return nil, nil
	}
	return d.fn(ctx, cfg, specSnap, statusSnap)
}

func TestDetectAllAggregatesFindingsAndErrors(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")

	d1 := detectorFunc{name: "d1", fn: func(context.Context, *config.Config, *spec.ManagedClusterSpec, *status.NodeStatus) ([]Finding, error) {
		return []Finding{{ID: "f1"}}, nil
	}}
	d2 := detectorFunc{name: "d2", fn: func(context.Context, *config.Config, *spec.ManagedClusterSpec, *status.NodeStatus) ([]Finding, error) {
		return nil, wantErr
	}}

	findings, err := DetectAll(context.Background(), []Detector{nil, d1, d2}, nil, nil, nil)
	if len(findings) != 1 {
		t.Fatalf("findings len=%d, want 1", len(findings))
	}
	if err == nil {
		t.Fatalf("err=nil, want non-nil")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("err=%v, want to contain %v", err, wantErr)
	}
}
