package v20260301

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/Azure/AKSFlexNode/components/kubeadm"
	"github.com/Azure/AKSFlexNode/components/kubelet"
	"github.com/Azure/AKSFlexNode/components/services/actions"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/systemd"
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
	if err := unmountBelow(config.KubeletRoot); err != nil {
		return nil, status.Errorf(codes.Internal, "unmount kubelet directories: %s", err)
	}

	// Step 3: Remove kubernetes directories and files.
	//         [reset] Deleting contents of directories: [...]
	//         [reset] Deleting files: [...]
	//
	// RemoveKubernetesDirs removes /var/lib/kubelet, /etc/kubernetes
	// (which includes pki/, manifests/, and all .conf files),
	// /etc/cni/net.d, /var/run/kubernetes, and /var/lib/cni.
	if err := kubeadm.RemoveKubernetesDirs(); err != nil {
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
