package v20260301

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/Azure/AKSFlexNode/components/kubelet"
	"github.com/Azure/AKSFlexNode/components/services/actions"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/systemd"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilpb"
)

type stopKubeletServiceAction struct {
	systemd systemd.Manager
}

func newStopKubeletServiceAction() (actions.Server, error) {
	return &stopKubeletServiceAction{
		systemd: systemd.New(),
	}, nil
}

var _ actions.Server = (*stopKubeletServiceAction)(nil)

func (s *stopKubeletServiceAction) ApplyAction(
	ctx context.Context,
	req *actions.ApplyActionRequest,
) (*actions.ApplyActionResponse, error) {
	msg, err := utilpb.AnyTo[*kubelet.StopKubeletService](req.GetItem())
	if err != nil {
		return nil, err
	}

	if err := s.stopKubelet(ctx); err != nil {
		return nil, err
	}

	item, err := anypb.New(msg)
	if err != nil {
		return nil, err
	}

	return actions.ApplyActionResponse_builder{Item: item}.Build(), nil
}

func (s *stopKubeletServiceAction) stopKubelet(ctx context.Context) error {
	if err := systemd.EnsureUnitStoppedAndDisabled(ctx, s.systemd, config.SystemdUnitKubelet); err != nil {
		return status.Errorf(codes.Internal, "stop/disable kubelet unit: %s", err)
	}

	return nil
}
