package v20260301

import (
	"go.goms.io/aks/AKSFlexNode/components/kubeadm"
	"go.goms.io/aks/AKSFlexNode/components/services/actions"
)

func init() {
	actions.MustRegister(
		newNodeJoinAction,
		&kubeadm.KubadmNodeJoin{},
	)
}
