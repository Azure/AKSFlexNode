package v20260301

import (
	"context"
	"errors"
	"fmt"
	"os"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/Azure/AKSFlexNode/components/cri"
	"github.com/Azure/AKSFlexNode/components/services/actions"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/systemd"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilpb"
)

// containerdConfigPaths lists the config paths managed by the CRI component
// that should be removed during a reset.
var containerdConfigPaths = []string{
	config.ContainerdConfigPath,
	config.ContainerdConfDropInDir,
}

type resetContainerdServiceAction struct {
	systemd systemd.Manager
}

func newResetContainerdServiceAction() (actions.Server, error) {
	return &resetContainerdServiceAction{
		systemd: systemd.New(),
	}, nil
}

var _ actions.Server = (*resetContainerdServiceAction)(nil)

func (r *resetContainerdServiceAction) ApplyAction(
	ctx context.Context,
	req *actions.ApplyActionRequest,
) (*actions.ApplyActionResponse, error) {
	msg, err := utilpb.AnyTo[*cri.ResetContainerdService](req.GetItem())
	if err != nil {
		return nil, err
	}

	if err := r.stopAndMaskContainerd(ctx); err != nil {
		return nil, err
	}

	if err := removeContainerdConfigs(); err != nil {
		return nil, status.Errorf(codes.Internal, "%s", err)
	}

	item, err := anypb.New(msg)
	if err != nil {
		return nil, err
	}

	return actions.ApplyActionResponse_builder{Item: item}.Build(), nil
}

// stopAndMaskContainerd idempotently stops, disables, and masks the containerd
// systemd unit so it cannot be accidentally restarted.
func (r *resetContainerdServiceAction) stopAndMaskContainerd(ctx context.Context) error {
	if err := systemd.EnsureUnitMasked(ctx, r.systemd, config.SystemdUnitContainerd); err != nil {
		return status.Errorf(codes.Internal, "mask containerd unit: %s", err)
	}

	return nil
}

// removeContainerdConfigs removes containerd configuration files and drop-in
// directories. It uses best-effort semantics: all paths are attempted, and
// errors (other than "not exist") are collected and returned together.
func removeContainerdConfigs() error {
	var errs []error
	for _, p := range containerdConfigPaths {
		if err := os.RemoveAll(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove %s: %w", p, err))
		}
	}

	return errors.Join(errs...)
}
