package kubeadm

import (
	_ "embed"
)

//go:embed assets/kubelet.service
var systemdUnitKubeletFile []byte

//go:embed assets/10-kubeadm.conf
var systemdDropInKubeadmFile []byte

const (
	systemdUnitKubelet   = "kubelet.service"
	systemdDropInKubeadm = "10-kubeadm.conf"
	dirVarLibKubelet     = "/var/lib/kubelet"
)
