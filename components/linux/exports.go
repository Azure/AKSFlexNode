package linux

import (
	v20260301 "go.goms.io/aks/AKSFlexNode/components/linux/v20260301"
	"go.goms.io/aks/AKSFlexNode/components/services/actions"
)

func init() {
	actions.MustRegister(
		newConfigureBaseOSAction,
		&v20260301.ConfigureBaseOS{},
	)
}
