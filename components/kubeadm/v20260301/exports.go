package v20260301

import (
	"github.com/Azure/AKSFlexNode/components/kubeadm"
	"github.com/Azure/AKSFlexNode/components/services/actions"
)

func init() {
	actions.MustRegister(
		newNodeJoinAction,
		&kubeadm.KubeadmNodeJoin{},
	)
}
