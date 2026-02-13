package kubeadm

import (
	v20260301 "go.goms.io/aks/AKSFlexNode/components/kubeadm/v20260301"
	"go.goms.io/aks/AKSFlexNode/components/services/actions"
)

func init() {
	actions.MustRegister(
		newNodeJoinAction,
		&v20260301.KubadmNodeJoin{},
	)
}
