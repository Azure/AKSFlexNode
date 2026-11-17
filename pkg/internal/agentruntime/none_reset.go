package agentruntime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Azure/AKSFlexNode/pkg/npd"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostnode"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/noneops"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
)

// NoneUpgradeArtifactsPresent detects recovery files left by an interrupted upgrade.
func NoneUpgradeArtifactsPresent() (bool, error) {
	for _, name := range []string{"none-upgrade-journal.json", "none-upgrade-config"} {
		path := filepath.Join(goalstates.AgentConfigDir, name)
		if _, err := os.Lstat(path); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("inspect none upgrade artifact %s: %w", path, err)
		}
	}
	return false, nil
}

// CleanupNoneUpgradeArtifacts removes only the SDK recovery files.
func CleanupNoneUpgradeArtifacts() phases.Task {
	return hostnode.Func("remove-none-upgrade-artifacts", func(context.Context) error {
		return removeNoneUpgradeArtifacts("")
	})
}

func (noneRuntime) Reset(log *slog.Logger) phases.Task {
	return phases.Serial(log,
		&noneResetTask{
			log:      log,
			teardown: &noneops.Teardown{Log: log},
		},
		phases.Parallel(log,
			reset.CleanupNetwork(log),
			reset.RemoveWireGuardKeys(log),
			cleanupLegacyBridgeCNI(log),
		),
		removeHostHelpers(log),
		reset.ReloadSystemd(log),
	)
}

type noneResetTask struct {
	log      *slog.Logger
	teardown *noneops.Teardown
}

func (*noneResetTask) Name() string { return "reset-none-node" }

func (t *noneResetTask) Do(ctx context.Context) error {
	if err := npd.RemoveHost(t.log, none.Root{HostRoot: t.teardown.Root}).Do(ctx); err != nil {
		return err
	}
	if err := phases.Serial(t.log, t.teardown.Stop(), t.teardown.RemoveConfig(), t.teardown.Destroy()).Do(ctx); err != nil {
		return fmt.Errorf("tear down host-native worker: %w", err)
	}
	if err := hostnode.ResetCNIState(t.teardown.Root); err != nil {
		return err
	}
	return removeNoneUpgradeArtifacts(t.teardown.Root)
}

func removeNoneUpgradeArtifacts(hostRoot string) error {
	dir := filepath.Join(hostRoot, goalstates.AgentConfigDir)
	for _, name := range []string{"none-upgrade-journal.json", "none-upgrade-config"} {
		path := filepath.Join(dir, name)
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove none upgrade artifact %s: %w", path, err)
		}
	}
	return nil
}
