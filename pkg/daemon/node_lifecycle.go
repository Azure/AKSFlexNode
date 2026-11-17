package daemon

import (
	"context"
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/daemon/internal/provider"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
)

// RunNodeLifecycle reconciles an OCI worker through the SDK. Pre-start retains
// the existing host-root migration before writing units that reference helpers.
func RunNodeLifecycle(ctx context.Context, log *slog.Logger, phase, slot string) error {
	if err := container.ValidSlot(slot); err != nil {
		return err
	}
	if phase == "pre-start" {
		if err := MigrateHostRoot(log); err != nil {
			return err
		}
	}
	return provider.RunContainerLifecycle(ctx, log, phase, slot)
}
