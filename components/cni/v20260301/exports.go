package v20260301

import (
	"github.com/Azure/AKSFlexNode/components/cni"
	"github.com/Azure/AKSFlexNode/components/services/actions"
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
