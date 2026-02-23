package v20260301

import (
	"go.goms.io/aks/AKSFlexNode/components/linux"
	"go.goms.io/aks/AKSFlexNode/components/services/actions"
)

func init() {
	actions.MustRegister(
		newConfigureBaseOSAction,
		&linux.ConfigureBaseOS{},
	)
}
