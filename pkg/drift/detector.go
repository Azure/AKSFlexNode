package drift

import (
	"context"
	"errors"

	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/spec"
	"go.goms.io/aks/AKSFlexNode/pkg/status"
)

// Finding represents a detected drift between desired spec and current node state.
// Findings should be small and composable so multiple detectors can return multiple findings.
type Finding struct {
	ID          string
	Title       string
	Details     string
	Remediation Remediation
}

// RemediationAction indicates what kind of action should be taken to remediate a drift.
// Empty value means "unspecified" and will fall back to legacy behavior.
type RemediationAction string

const (
	RemediationActionUnspecified       RemediationAction = ""
	RemediationActionKubernetesUpgrade RemediationAction = "kubernetes-upgrade"
)

// Remediation describes what the agent should do to address a drift.
// This is intentionally a minimal set of knobs; it can be extended as new remediation
// types are needed (e.g., restart services, rewrite config files, run targeted installers).
type Remediation struct {
	// Action indicates the remediation strategy.
	// If unset, the finding is informational and won't trigger remediation.
	Action RemediationAction

	// KubernetesVersion, when set, indicates the desired Kubernetes version that should be
	// used during bootstrap (e.g., to trigger kubelet upgrade).
	KubernetesVersion string
}

// Detector compares desired spec and current status and returns any drift findings.
// Detectors should be pure (no side effects) and fast.
type Detector interface {
	Name() string
	Detect(ctx context.Context, cfg *config.Config, specSnap *spec.ManagedClusterSpec, statusSnap *status.NodeStatus) ([]Finding, error)
}

// DetectAll runs all detectors, returning aggregated findings.
// If some detectors error, the error is returned (joined) along with any findings.
func DetectAll(
	ctx context.Context,
	detectors []Detector,
	cfg *config.Config,
	specSnap *spec.ManagedClusterSpec,
	statusSnap *status.NodeStatus,
) ([]Finding, error) {
	var findings []Finding
	var errs []error

	for _, d := range detectors {
		if d == nil {
			continue
		}
		f, err := d.Detect(ctx, cfg, specSnap, statusSnap)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if len(f) > 0 {
			findings = append(findings, f...)
		}
	}

	if len(errs) > 0 {
		return findings, errors.Join(errs...)
	}
	return findings, nil
}
