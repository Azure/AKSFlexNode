package v20260301

import (
	"context"
	"os/exec"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"
	utilexec "k8s.io/utils/exec"

	"github.com/Azure/AKSFlexNode/components/kubeadm"
	"github.com/Azure/AKSFlexNode/components/services/actions"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/systemd"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilpb"
)

type nodeResetAction struct {
	systemd        systemd.Manager
	kubeadmCommand string // to allow overriding in unit test
}

func newNodeResetAction() (actions.Server, error) {
	return &nodeResetAction{
		systemd: systemd.New(),
	}, nil
}

var _ actions.Server = (*nodeResetAction)(nil)

func (n *nodeResetAction) ApplyAction(
	ctx context.Context,
	req *actions.ApplyActionRequest,
) (*actions.ApplyActionResponse, error) {
	msg, err := utilpb.AnyTo[*kubeadm.KubeadmNodeReset](req.GetItem())
	if err != nil {
		return nil, err
	}

	if err := n.runReset(ctx); err != nil {
		return nil, err
	}

	if err := n.stopAndMaskKubelet(ctx); err != nil {
		return nil, err
	}

	if err := kubeadm.CleanKubernetesDirs(); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	item, err := anypb.New(msg)
	if err != nil {
		return nil, err
	}

	return actions.ApplyActionResponse_builder{Item: item}.Build(), nil
}

func (n *nodeResetAction) resolveKubeadmBinary() (string, error) {
	if n.kubeadmCommand != "" {
		return n.kubeadmCommand, nil
	}

	return exec.LookPath("kubeadm")
}

func (n *nodeResetAction) runReset(ctx context.Context) error {
	kubeadmBinary, err := n.resolveKubeadmBinary()
	if err != nil {
		return status.Errorf(codes.Internal, "resolve kubeadm binary: %s", err)
	}

	// --force skips the interactive confirmation prompt and proceeds even
	// when the node is unreachable by the control plane.
	// -v 5 provides verbose output for debugging.
	if err := utilexec.New().CommandContext(
		ctx, kubeadmBinary, "reset", "--force", "-v", "5",
	).Run(); err != nil {
		return status.Errorf(codes.Internal, "kubeadm reset: %s", err)
	}

	return nil
}

// stopAndMaskKubelet idempotently stops, disables, and masks the kubelet
// systemd unit so it cannot be accidentally restarted before a new join.
func (n *nodeResetAction) stopAndMaskKubelet(ctx context.Context) error {
	if err := systemd.EnsureUnitMasked(ctx, n.systemd, config.SystemdUnitKubelet); err != nil {
		return status.Errorf(codes.Internal, "mask kubelet unit: %s", err)
	}

	return nil
}
