package v20260301

import (
	"go.goms.io/aks/AKSFlexNode/components/npd"
	"go.goms.io/aks/AKSFlexNode/components/services/actions"
)

func init() {
	actions.MustRegister(
		newDownloadNodeProblemDetectorAction,
		&npd.DownloadNodeProblemDetector{},
	)

	actions.MustRegister(
		newStartNodeProblemDetectorAction,
		&npd.StartNodeProblemDetector{},
	)
}
