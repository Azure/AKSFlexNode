package drift

import (
	"context"
	"fmt"
	"strings"

	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/spec"
	"go.goms.io/aks/AKSFlexNode/pkg/status"
)

const KubernetesVersionFindingID = "kubernetes-version"

type KubernetesVersionDetector struct{}

func NewKubernetesVersionDetector() *KubernetesVersionDetector {
	return &KubernetesVersionDetector{}
}

func (d *KubernetesVersionDetector) Name() string {
	return "KubernetesVersionDetector"
}

func (d *KubernetesVersionDetector) Detect(
	ctx context.Context,
	_ *config.Config,
	specSnap *spec.ManagedClusterSpec,
	statusSnap *status.NodeStatus,
) ([]Finding, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}

	if specSnap == nil || statusSnap == nil {
		return nil, nil
	}

	desired := strings.TrimSpace(specSnap.CurrentKubernetesVersion)
	if desired == "" {
		desired = strings.TrimSpace(specSnap.KubernetesVersion)
	}
	if desired == "" {
		return nil, nil
	}

	current := strings.TrimSpace(statusSnap.KubeletVersion)
	if current == "" || current == "unknown" {
		return nil, nil
	}

	cmp, ok := compareMajorMinor(current, desired)
	if ok {
		// Never downgrade via drift remediation. If the node is already newer than the desired
		// version, treat it as non-actionable drift.
		if cmp >= 0 {
			return nil, nil
		}
	} else {
		// If we can't parse versions, fall back to string major.minor comparison.
		// This keeps the detector safe (won't trigger if they look equal) while still working
		// for common version formats.
		if majorMinor(current) == majorMinor(desired) {
			return nil, nil
		}
		// If we can't compare ordering, don't remediate automatically (avoid accidental downgrade).
		return nil, nil
	}

	return []Finding{
		{
			ID:      KubernetesVersionFindingID,
			Title:   "Kubernetes version drift",
			Details: fmt.Sprintf("kubelet=%q desired=%q", current, desired),
			Remediation: Remediation{
				Action:            RemediationActionKubernetesUpgrade,
				KubernetesVersion: desired,
			},
		},
	}, nil
}
