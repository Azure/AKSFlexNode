package v20260301

import (
	"context"
	"fmt"
	"os"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/Azure/AKSFlexNode/components/kubelet"
	"github.com/Azure/AKSFlexNode/components/services/actions"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/systemd"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilmount"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilpb"
)

type resetKubeletAction struct {
	systemd systemd.Manager
}

func newResetKubeletAction() (actions.Server, error) {
	return &resetKubeletAction{
		systemd: systemd.New(),
	}, nil
}

var _ actions.Server = (*resetKubeletAction)(nil)

// resetDirs are directories whose contents are deleted during reset.
// This mirrors the kubeadm reset behaviour minus etcd:
//
//	[reset] Deleting contents of directories: [/etc/kubernetes/manifests /var/lib/kubelet /etc/kubernetes/pki]
//
// We additionally remove CNI state and runtime dirs that the kubelet
// component manages outside of kubeadm.
var resetDirs = []string{
	config.KubeletStaticPodPath, // /etc/kubernetes/manifests
	config.KubeletRoot,          // /var/lib/kubelet
	config.KubernetesPKIDir,     // /etc/kubernetes/pki
	config.DefaultCNIConfigDir,  // /etc/cni/net.d
	config.KubernetesRunDir,     // /var/run/kubernetes
	config.CNIStateDir,          // /var/lib/cni
}

// resetFiles are individual files deleted during reset.
// kubeadm reset deletes:
//
//	[reset] Deleting files: [/etc/kubernetes/admin.conf /etc/kubernetes/super-admin.conf
//	  /etc/kubernetes/kubelet.conf /etc/kubernetes/bootstrap-kubelet.conf
//	  /etc/kubernetes/controller-manager.conf /etc/kubernetes/scheduler.conf]
//
// Only kubelet.conf and bootstrap-kubelet.conf are relevant for a
// worker node; the others are control-plane files included for
// completeness (removal is a no-op when they don't exist).
var resetFiles = []string{
	config.KubernetesConfigDir + "/admin.conf",
	config.KubernetesConfigDir + "/super-admin.conf",
	config.KubernetesConfigDir + "/kubelet.conf",
	config.KubernetesConfigDir + "/bootstrap-kubelet.conf",
	config.KubernetesConfigDir + "/controller-manager.conf",
	config.KubernetesConfigDir + "/scheduler.conf",
}

func (r *resetKubeletAction) ApplyAction(
	ctx context.Context,
	req *actions.ApplyActionRequest,
) (*actions.ApplyActionResponse, error) {
	msg, err := utilpb.AnyTo[*kubelet.ResetKubelet](req.GetItem())
	if err != nil {
		return nil, err
	}

	// Step 1: Stop and mask the kubelet service.
	//         [reset] Stopping the kubelet service
	if err := r.stopAndMaskKubelet(ctx); err != nil {
		return nil, err
	}

	// Step 2: Unmount all mount points under /var/lib/kubelet.
	//         [reset] Unmounting mounted directories in "/var/lib/kubelet"
	if err := utilmount.UnmountBelow(config.KubeletRoot); err != nil {
		return nil, status.Errorf(codes.Internal, "unmount kubelet directories: %s", err)
	}

	// Step 3: Remove directories and files.
	//         [reset] Deleting contents of directories: [...]
	//         [reset] Deleting files: [...]
	if err := removeAll(resetDirs, resetFiles); err != nil {
		return nil, status.Errorf(codes.Internal, "%s", err)
	}

	item, err := anypb.New(msg)
	if err != nil {
		return nil, err
	}

	return actions.ApplyActionResponse_builder{Item: item}.Build(), nil
}

// stopAndMaskKubelet idempotently stops, disables, and masks the kubelet
// systemd unit so it cannot be accidentally restarted before a new join.
func (r *resetKubeletAction) stopAndMaskKubelet(ctx context.Context) error {
	if err := systemd.EnsureUnitMasked(ctx, r.systemd, config.SystemdUnitKubelet); err != nil {
		return status.Errorf(codes.Internal, "mask kubelet unit: %s", err)
	}

	return nil
}

// removeAll removes a list of directories and individual files.
// Removal is best-effort: every path is attempted even if earlier
// removals fail. The first error encountered is returned.
func removeAll(dirs []string, files []string) error {
	var errs []error

	for _, dir := range dirs {
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, fmt.Errorf("remove directory %s: %w", dir, err))
		}
	}

	for _, file := range files {
		if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove file %s: %w", file, err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("cleanup kubernetes state: %w", errs[0])
	}

	return nil
}
