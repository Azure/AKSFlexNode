package kubeadm

import (
	"fmt"
	"os"

	"github.com/Azure/AKSFlexNode/pkg/config"
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

// RemoveKubernetesDirs removes directories created during a kubeadm join.
// Removal is best-effort across all paths: every directory is attempted
// even if earlier ones fail. The first error encountered is returned.
//
// FIXME: find a better place to put this function for reusing with kubelet component
func RemoveKubernetesDirs() error {
	var errs []error
	for _, dir := range kubernetesDirs {
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", dir, err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("cleanup kubernetes directories: %w", errs[0])
	}

	return nil
}
