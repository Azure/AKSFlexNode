package drift

import (
	"context"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/spec"
	"github.com/Azure/AKSFlexNode/pkg/status"
)

const NodeRebootFindingID = "node-reboot"

type RebootDetector struct{}

func NewRebootDetector() *RebootDetector {
	return &RebootDetector{}
}

func (d *RebootDetector) Name() string {
	return "RebootDetector"
}

func (d *RebootDetector) Detect(
	ctx context.Context,
	_ *config.Config,
	_ *spec.ManagedClusterSpec,
	statusSnap *status.NodeStatus,
) ([]Finding, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	if statusSnap == nil {
		return nil, nil
	}

	if !statusSnap.NeedReboot {
		return nil, nil
	}

	return []Finding{
		{
			ID:      NodeRebootFindingID,
			Title:   "Node reboot required",
			Details: "Node status indicates a reboot is needed",
			Remediation: Remediation{
				Action: RemediationActionReboot,
			},
		},
	}, nil
}
