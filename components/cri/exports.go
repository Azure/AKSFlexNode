package cri

import (
	v20260301 "go.goms.io/aks/AKSFlexNode/components/cri/v20260301"
	"go.goms.io/aks/AKSFlexNode/components/services/actions"
)

func init() {
	actions.MustRegister(
		newDownloadCRIBinariesAction,
		&v20260301.DownloadCRIBinaries{},
	)
}
