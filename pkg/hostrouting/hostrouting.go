package hostrouting

import (
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// Configure returns a single phases.Task that installs static routes and
// verifies no route overlap exists. It sequentially runs ConfigureStaticRoutes
// then CheckRouteOverlap. Both oneshot units are ordered
// Before=systemd-nspawn@.service so the kernel route table is correct before
// the container boots.
func Configure(cfg *config.Config, logger *slog.Logger) phases.Task {
	return phases.Serial(logger,
		ConfigureStaticRoutes(cfg, logger),
		CheckRouteOverlap(cfg, logger),
	)
}
