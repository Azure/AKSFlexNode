package kubebins

import (
	v20260301 "go.goms.io/aks/AKSFlexNode/components/kubebins/v20260301"
	"go.goms.io/aks/AKSFlexNode/components/services/actions"
)

func init() {
	actions.MustRegister(
		newDownloadKubeBinariesAction,
		&v20260301.DownloadKubeBinaries{},
	)
}
