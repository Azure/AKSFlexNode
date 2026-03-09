package v20260301

import (
	_ "embed"
)

//go:embed assets/kubelet.service
var systemdUnitKubeletFile []byte

//go:embed assets/10-kubeadm.conf
var systemdDropInKubeadmFile []byte

const systemdDropInKubeadm = "10-kubeadm.conf"
