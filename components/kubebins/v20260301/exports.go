package v20260301

import (
	"go.goms.io/aks/AKSFlexNode/components/kubebins"
	"go.goms.io/aks/AKSFlexNode/components/services/actions"
)

func init() {
	actions.MustRegister(
		newDownloadKubeBinariesAction,
		&kubebins.DownloadKubeBinaries{},
	)
}
