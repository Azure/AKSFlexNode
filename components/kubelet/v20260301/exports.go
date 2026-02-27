package v20260301

import (
	"go.goms.io/aks/AKSFlexNode/components/kubelet"
	"go.goms.io/aks/AKSFlexNode/components/services/actions"
)

func init() {
	actions.MustRegister(
		newStartKubeletServiceAction,
		&kubelet.StartKubeletService{},
	)
}
