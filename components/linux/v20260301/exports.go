package v20260301

import (
	"github.com/Azure/AKSFlexNode/components/linux"
	"github.com/Azure/AKSFlexNode/components/services/actions"
)

func init() {
	actions.MustRegister(
		newConfigureBaseOSAction,
		&linux.ConfigureBaseOS{},
	)
}
