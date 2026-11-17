package npd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// Stop ends NPD before a host-native repave replaces kubelet's credentials.
func Stop(log *slog.Logger, root isolation.NodeRoot, nodeStart *goalstates.NodeStart) phases.Task {
	return &stopTask{log: log, root: root, run: nodeStart.Exec}
}

// RemoveHost removes NPD artifacts that would disappear with a guest rootfs.
func RemoveHost(log *slog.Logger, root isolation.NodeRoot) phases.Task {
	return &stopTask{log: log, root: root, run: func(ctx context.Context, log *slog.Logger, args ...string) (string, error) {
		return utilexec.OutputCmd(ctx, log, args[0], args[1:]...)
	}, remove: true}
}

type stopTask struct {
	log    *slog.Logger
	root   isolation.NodeRoot
	run    func(context.Context, *slog.Logger, ...string) (string, error)
	remove bool
}

func (*stopTask) Name() string { return "stop-npd" }
func (t *stopTask) Do(ctx context.Context) error {
	unit := t.root.Path("/etc/systemd/system/" + systemdUnitNPD)
	_, err := os.Stat(unit)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		action := []string{"stop", systemdUnitNPD}
		if t.remove {
			action = []string{"disable", "--now", systemdUnitNPD}
		}
		if _, err := t.run(ctx, t.log, append([]string{"systemctl"}, action...)...); err != nil {
			return fmt.Errorf("stop NPD: %w", err)
		}
	}
	if !t.remove {
		return nil
	}
	for _, path := range []string{unit, t.root.Path(npdBinaryPath), t.root.Path(npdConfigPath)} {
		if err := os.Remove(filepath.Clean(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
