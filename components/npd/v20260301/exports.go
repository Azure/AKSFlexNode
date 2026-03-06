package v20260301

import (
	"github.com/Azure/AKSFlexNode/components/npd"
	"github.com/Azure/AKSFlexNode/components/services/actions"
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
