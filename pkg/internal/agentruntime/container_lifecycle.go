package agentruntime

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/unbounded/pkg/agent/containerops"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
	"github.com/Azure/unbounded/pkg/agent/nodelifecycle"
)

type containerController struct {
	backend *container.Backend
}

func (c containerController) Regenerate(ctx context.Context, slot string) error {
	return c.backend.Regenerate(ctx, slot)
}

func (c containerController) Running(ctx context.Context, slot string) (bool, string, error) {
	// The supervisor may still be activating in ExecStartPost while runc is running.
	status, err := c.backend.RuntimeStatus(ctx, isolation.SlotName(slot))
	return status.Running, status.Detail, err
}

func (c containerController) Restart(ctx context.Context, slot string) error {
	return c.backend.Restart(ctx, isolation.SlotName(slot))
}

// RunContainerLifecycle handles the OCI supervisor's SDK lifecycle hooks.
func RunContainerLifecycle(ctx context.Context, log *slog.Logger, phase, slot string) error {
	if err := container.ValidSlot(slot); err != nil {
		return err
	}

	gpu := containerops.NVIDIAHooks{}
	backend := &container.Backend{
		Log:           log,
		RefreshGrants: containerops.RefreshNVIDIAGrants(gpu),
	}
	lifecycle := &nodelifecycle.Lifecycle{
		Log:        log,
		Controller: containerController{backend: backend},
		Peer:       container.OtherSlot,
		ReloadSystemd: func(ctx context.Context) error {
			return utilexec.ReloadSystemd(ctx, log)
		},
		AfterStart: func(ctx context.Context, slot string) error {
			return containerops.ReconcileNVIDIA(ctx, log, backend, slot, gpu)
		},
	}

	switch phase {
	case "pre-start":
		return lifecycle.PreStart(ctx, slot)
	case "post-start":
		return lifecycle.PostStart(ctx, slot)
	case "reconcile":
		return lifecycle.Reconcile(ctx, slot)
	default:
		return fmt.Errorf("unknown node lifecycle phase %q", phase)
	}
}
