package v20260301

import (
	"github.com/Azure/AKSFlexNode/components/cri"
	"github.com/Azure/AKSFlexNode/components/services/actions"
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
