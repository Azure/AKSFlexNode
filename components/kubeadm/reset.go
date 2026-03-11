package kubeadm

import (
	"errors"
	"fmt"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
)

// kubernetesDirs are directories created during a kubeadm join that must be
// removed to fully reset the node. kubeadm reset cleans most of these, but
// may leave some behind depending on the runtime state.
//
// ref: https://kubernetes.io/docs/reference/setup-tools/kubeadm/kubeadm-reset/
var kubernetesDirs = []string{
	config.KubeletRoot,         // /var/lib/kubelet
	config.KubernetesConfigDir, // /etc/kubernetes — config, PKI, manifests
	config.DefaultCNIConfigDir, // /etc/cni/net.d — CNI configuration written during join
	config.KubernetesRunDir,    // /var/run/kubernetes — runtime sockets and PID files
	config.CNIStateDir,         // /var/lib/cni — CNI state data
}

// CleanKubernetesDirs removes files under the kubernetes directories.
// It aggregates errors for all directories.
//
// FIXME: find a better place to put this function for reusing with kubelet component
func CleanKubernetesDirs() error {
	var cleanErr error
	for _, dir := range kubernetesDirs {
		if err := utilio.CleanDir(dir); err != nil {
			cleanErr = errors.Join(cleanErr, fmt.Errorf("remove %s: %w", dir, err))
		}
	}

	return cleanErr
}
