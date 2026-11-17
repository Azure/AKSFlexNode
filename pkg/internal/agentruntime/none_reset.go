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

func cleanupNoneUpgradeArtifacts(root string) phases.Task {
	return hostnode.Func("remove-none-upgrade-artifacts", func(context.Context) error {
		return removeNoneUpgradeArtifacts(root)
	})
}

type noneTeardown struct {
	log *slog.Logger
	sdk *noneops.Teardown
}

func (t *noneTeardown) Present() bool { return t.sdk.Present() }

func (t *noneTeardown) Stop() phases.Task { return t.sdk.Stop() }

func (t *noneTeardown) Destroy() phases.Task {
	return hostnode.Func("reset-none-node", func(ctx context.Context) error {
		if err := npd.RemoveHost(t.log, none.Root{HostRoot: t.sdk.Root}).Do(ctx); err != nil {
			return err
		}
		if err := t.sdk.RemoveConfig().Do(ctx); err != nil {
			return fmt.Errorf("remove host-native configuration: %w", err)
		}

		err := t.destroy(ctx)
		if err == nil {
			return nil
		}
		// SDK Destroy may remove all ownership traces before a later unmount or
		// data cleanup fails. Restore its layout directory for a fresh retry.
		path := filepath.Dir(t.sdk.Layout.CurrentLink())
		if restoreErr := os.MkdirAll(path, 0o750); restoreErr != nil {
			return errors.Join(err, fmt.Errorf("restore none ownership trace %s: %w", path, restoreErr))
		}
		return err
	})
}

func (t *noneTeardown) destroy(ctx context.Context) error {
	if err := t.sdk.Destroy().Do(ctx); err != nil {
		return fmt.Errorf("destroy host-native worker: %w", err)
	}
	if err := hostnode.ResetCNIState(t.sdk.Root); err != nil {
		return fmt.Errorf("clean host-native CNI state: %w", err)
	}
	return removeNoneUpgradeArtifacts(t.sdk.Root)
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
