package v20260301

import (
	"context"

	"go.goms.io/aks/AKSFlexNode/components/services/actions"
	"go.goms.io/aks/AKSFlexNode/pkg/systemd"
)

type startKubeletServiceAction struct {
	systemd systemd.Manager
}

func newStartKubeletServiceAction() (actions.Server, error) {
	systemdManager := systemd.New()

	return &startKubeletServiceAction{
		systemd: systemdManager,
	}, nil
}

var _ actions.Server = (*startKubeletServiceAction)(nil)

func (s *startKubeletServiceAction) ApplyAction(
	ctx context.Context,
	req *actions.ApplyActionRequest,
) (*actions.ApplyActionResponse, error) {
	panic("unimplemented")
}
