package kubeadm

func (x *KubeadmNodeJoin) Redact() {
	if x == nil {
		return
	}

	x.GetSpec().Redact()
}

func (x *KubeadmNodeJoinSpec) Redact() {
	if x == nil {
		return
	}

	x.GetKubelet().Redact()
}

func (x *Kubelet) Redact() {
	if x == nil {
		return
	}

	x.GetBootstrapAuthInfo().Redact()
}

func (x *NodeAuthInfo) Redact() {
	if x == nil {
		return
	}

	if x.HasToken() {
		x.SetToken("")
	}
}
