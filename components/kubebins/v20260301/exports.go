package v20260301

import (
	"github.com/Azure/AKSFlexNode/components/kubebins"
	"github.com/Azure/AKSFlexNode/components/services/actions"
)

func init() {
	actions.MustRegister(
		newDownloadKubeBinariesAction,
		&kubebins.DownloadKubeBinaries{},
	)
}
