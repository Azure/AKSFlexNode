package v20260301

import (
	"go.goms.io/aks/AKSFlexNode/components/cri"
	"go.goms.io/aks/AKSFlexNode/components/services/actions"
)

func init() {
	actions.MustRegister(
		newDownloadCRIBinariesAction,
		&cri.DownloadCRIBinaries{},
	)

	actions.MustRegister(
		newStartContainerdServiceAction,
		&cri.StartContainerdService{},
	)
}
