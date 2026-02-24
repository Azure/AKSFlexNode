package kubelet

func (x *StartKubeletService) Redact() {

}

func (x *StartKubeletServiceSpec) Redact() {
	x.GetNodeAuthInfo().Redact()
}

func (x *NodeAuthInfo) Redact() {
	if x.HasArcCredential() {
		x.GetArcCredential().Redact()
	}
	if x.HasMsiCredential() {
		x.GetMsiCredential().Redact()
	}
	if x.HasServicePrincipalCredential() {
		x.GetServicePrincipalCredential().Redact()
	}
	if x.HasBootstrapTokenCredential() {
		x.GetBootstrapTokenCredential().Redact()
	}
}

func (x *KubeletArcCredential) Redact() {}

func (x *KubeletMSICredential) Redact() {}

func (x *KubeletServicePrincipalCredential) Redact() {
	x.SetClientSecret(nil)
}

func (x *KubeletBootstrapTokenCredential) Redact() {
	x.SetToken("")
}
