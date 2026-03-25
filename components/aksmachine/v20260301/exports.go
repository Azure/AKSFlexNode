package v20260301

import (
	"github.com/Azure/AKSFlexNode/components/aksmachine"
	"github.com/Azure/AKSFlexNode/components/services/actions"
)

func init() {
	actions.MustRegister(
		newEnsureMachineAction,
		&aksmachine.EnsureMachine{},
	)
}
