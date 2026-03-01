package kubeadm

import (
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	corev1 "k8s.io/api/core/v1"
)

func (x *Kubelet) GetK8SRegisterTaints() []corev1.Taint {
	rv := make([]corev1.Taint, 0, len(x.GetRegisterWithTaints()))
	for _, taint := range x.GetRegisterWithTaints() {
		rv = append(rv, corev1.Taint{
			Key:    taint.GetKey(),
			Value:  taint.GetValue(),
			Effect: corev1.TaintEffect(taint.GetEffect()),
		})
	}
	return rv
}

func (x *Kubelet) AddK8SRegisterTaints(xs ...corev1.Taint) {
	taints := x.GetRegisterWithTaints()
	for _, o := range xs {
		taints = append(taints, Taint_builder{
			Key:    to.Ptr(o.Key),
			Value:  to.Ptr(o.Value),
			Effect: to.Ptr(string(o.Effect)),
		}.Build())
	}
	x.SetRegisterWithTaints(taints)
}
