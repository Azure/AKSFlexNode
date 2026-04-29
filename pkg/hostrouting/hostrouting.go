package hostrouting

import (
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// Tasks returns a single phases.Task that sequentially runs
// ConfigureStaticRoutes then CheckRouteOverlap. Both oneshot units are ordered
// Before=systemd-nspawn@.service so the host route table is correct before the
// container boots.
func Tasks(cfg *config.Config, logger *slog.Logger) phases.Task {
	return phases.Serial(logger,
		ConfigureStaticRoutes(cfg, logger),
		CheckRouteOverlap(cfg, logger),
	)
}
