package nodelifecycle

import (
	"context"
	"log/slog"
	"os"

	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/unbounded/pkg/agent/containerops"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
	shared "github.com/Azure/unbounded/pkg/agent/nodelifecycle"
	"github.com/spf13/cobra"
)

type lifecycle interface {
	PreStart(context.Context, string) error
	PostStart(context.Context, string) error
	Reconcile(context.Context, string) error
}

// NewCommand exposes the lifecycle hooks used by container supervising units.
func NewCommand() *cobra.Command {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	return newCommand(func() lifecycle { return newLifecycle(log, containerops.NVIDIAHooks{}) })
}

func newCommand(factory func() lifecycle) *cobra.Command {
	cmd := &cobra.Command{Use: "node-lifecycle", Short: "Run internal node lifecycle hooks", Hidden: true}
	for _, phase := range []struct {
		name string
		run  func(lifecycle, context.Context, string) error
	}{
		{"pre-start", lifecycle.PreStart},
		{"post-start", lifecycle.PostStart},
		{"reconcile", lifecycle.Reconcile},
	} {
		cmd.AddCommand(&cobra.Command{
			Use: phase.name + " SLOT", Hidden: true, Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				if err := container.ValidSlot(args[0]); err != nil {
					return err
				}
				return phase.run(factory(), cmd.Context(), args[0])
			},
		})
	}
	return cmd
}

type controller struct{ backend *container.Backend }

func (c controller) Regenerate(ctx context.Context, slot string) error {
	return c.backend.Regenerate(ctx, slot)
}

func (c controller) Running(ctx context.Context, slot string) (bool, string, error) {
	// ExecStartPost runs while the unit is activating. Only the runtime can
	// report readiness here; unit-level Status would wait on this hook itself.
	status, err := c.backend.RuntimeStatus(ctx, isolation.SlotName(slot))
	return status.Running, status.Detail, err
}

func (c controller) Restart(ctx context.Context, slot string) error {
	return c.backend.Restart(ctx, isolation.SlotName(slot))
}

func newLifecycle(log *slog.Logger, gpu containerops.NVIDIAHooks) *shared.Lifecycle {
	backend := &container.Backend{Log: log, RefreshGrants: containerops.RefreshNVIDIAGrants(gpu)}
	return &shared.Lifecycle{
		Log: log, Controller: controller{backend}, Peer: container.OtherSlot,
		AfterStart: func(ctx context.Context, slot string) error {
			return containerops.ReconcileNVIDIA(ctx, log, backend, slot, gpu)
		},
		ReloadSystemd: func(ctx context.Context) error { return utilexec.ReloadSystemd(ctx, log) },
	}
}
