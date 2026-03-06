package v20260301

import (
	"github.com/Azure/AKSFlexNode/components/kubelet"
	"github.com/Azure/AKSFlexNode/components/services/actions"
)

func init() {
	actions.MustRegister(
		newStartKubeletServiceAction,
		&kubelet.StartKubeletService{},
	)
}
