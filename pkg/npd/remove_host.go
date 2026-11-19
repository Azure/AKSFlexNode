package npd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// RemoveHost removes the NPD service and artifacts from a host-native worker.
func RemoveHost(log *slog.Logger, root isolation.NodeRoot) phases.Task {
	return &removeHostTask{log: log, root: root, run: utilexec.OutputCmd}
}

type removeHostTask struct {
	log  *slog.Logger
	root isolation.NodeRoot
	run  func(context.Context, *slog.Logger, string, ...string) (string, error)
}

func (*removeHostTask) Name() string { return "remove-host-npd" }

func (t *removeHostTask) Do(ctx context.Context) error {
	unit := t.root.Path(filepath.Join("/etc/systemd/system", systemdUnitNPD))
	_, err := os.Lstat(unit)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// Offline bootstrap may not have installed an NPD unit.
	case err != nil:
		return fmt.Errorf("stat NPD unit: %w", err)
	default:
		if _, err := t.run(ctx, t.log, "systemctl", "disable", "--now", systemdUnitNPD); err != nil {
			return fmt.Errorf("disable NPD: %w", err)
		}
	}

	for _, path := range []string{t.root.Path(npdBinaryPath), t.root.Path(npdConfigPath), unit} {
		if err := os.Remove(filepath.Clean(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove NPD artifact %s: %w", path, err)
		}
	}

	return nil
}
