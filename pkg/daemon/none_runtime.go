package daemon

import (
	"context"
	"log/slog"
	"path/filepath"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/npd"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/noneops"
)

func newNoneRuntime(p noneops.Params) *noneops.Node {
	// Keep the SDK's journal and snapshot within Flex's reset ownership.
	p.ConfigSnapshotDir = filepath.Join(p.HostRoot, config.ConfigDir, "none-upgrade-config")
	return noneops.New(p)
}

func prepareNoneDependencies(ctx context.Context, log *slog.Logger, cfg *config.Config) error {
	return configureNoneDependencies(log, noneRequiredServices(cfg)).Do(ctx)
}

// NPD is installed after the SDK selects a healthy runtime. Recovery can select
// the source again, including for a settings-only repave sharing its binaries.
func installNoneNPD(ctx context.Context, log *slog.Logger, cfg *config.Config, gs *goalstates.MachineGoalState) error {
	return npd.Download(log, cfg, gs.RootFS.Root()).Do(ctx)
}
