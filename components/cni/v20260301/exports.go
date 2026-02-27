package v20260301

import (
	"go.goms.io/aks/AKSFlexNode/components/cni"
	"go.goms.io/aks/AKSFlexNode/components/services/actions"
)

func init() {
	actions.MustRegister(
		newDownloadCNIBinariesAction,
		&cni.DownloadCNIBinaries{},
	)
	actions.MustRegister(
		newConfigureCNIAction,
		&cni.ConfigureCNI{},
	)
}
