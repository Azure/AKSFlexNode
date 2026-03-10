package v20260301

import (
	"github.com/Azure/AKSFlexNode/components/arc"
	"github.com/Azure/AKSFlexNode/components/services/actions"
)

func init() {
	actions.MustRegister(
		newInstallArcAction,
		&arc.InstallArc{},
	)
}
