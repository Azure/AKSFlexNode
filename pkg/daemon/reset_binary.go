package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type restoreAgentBinaryTask struct {
	log            *slog.Logger
	paths          agentUpgradePaths
	lockPath       string
	inspectService func(context.Context, *slog.Logger, string) (bool, error)
}

// RestoreAgentBinary restores a direct executable after UninstallService.
func RestoreAgentBinary(log *slog.Logger) phases.Task {
	return &restoreAgentBinaryTask{
		log:            log,
		paths:          defaultAgentUpgradePaths(),
		lockPath:       agentUpgradeLockPath,
		inspectService: inspectAgentServiceActive,
	}
}

func (t *restoreAgentBinaryTask) Name() string { return "restore-agent-binary" }

func (t *restoreAgentBinaryTask) Do(ctx context.Context) error {
	lock, err := agentbinary.AcquireHostActivationLock(t.lockPath)
	if err != nil {
		return fmt.Errorf("lock agent binary restoration: %w", err)
	}
	defer lock.Close() //nolint:errcheck // closing releases the activation lock

	for _, unit := range []string{ServiceUnitName, recoveryServiceUnitName} {
		active, err := t.inspectService(ctx, t.log, unit)
		if err != nil {
			return fmt.Errorf("inspect %s before restoring agent binary: %w", unit, err)
		}
		if active {
			return fmt.Errorf("cannot restore agent binary while %s is active", unit)
		}
	}
	if err := restoreAgentBinary(t.paths); err != nil {
		return err
	}
	t.log.Info("restored direct agent binary", "path", t.paths.BinaryPath)
	return nil
}

func restoreAgentBinary(paths agentUpgradePaths) error {
	if err := validateAgentUpgradePaths(paths); err != nil {
		return err
	}
	if paths == defaultAgentUpgradePaths() {
		if os.Geteuid() != 0 {
			return fmt.Errorf("agent binary restoration requires root privileges")
		}
		if err := validateRootOwnedAgentUpgradePaths(paths); err != nil {
			return err
		}
	}
	layoutDir := filepath.Dir(paths.BluePath)
	info, err := os.Lstat(layoutDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect managed binary directory: %w", err)
	}
	if err == nil && !info.IsDir() {
		return fmt.Errorf("managed binary directory %s is not a directory", layoutDir)
	}

	info, err = os.Lstat(paths.BinaryPath)
	if err != nil {
		return fmt.Errorf("inspect agent binary: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(paths.BinaryPath)
		if err != nil {
			return fmt.Errorf("read agent binary link: %w", err)
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(paths.BinaryPath), target)
		}
		if target != paths.CurrentPath {
			return fmt.Errorf("agent binary link does not point to %s", paths.CurrentPath)
		}
		active, err := resolvedExecutable(paths.BinaryPath)
		if err != nil {
			return fmt.Errorf("resolve active agent binary: %w", err)
		}
		if active != paths.BluePath && active != paths.GreenPath {
			return fmt.Errorf("active agent binary is outside the managed slots")
		}
		// Replace the public link atomically before removing any slot, including
		// the executable of this reset process.
		if err := copyExecutable(active, paths.BinaryPath); err != nil {
			return fmt.Errorf("restore direct agent binary: %w", err)
		}
	} else if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("agent binary %s is not a regular executable file", paths.BinaryPath)
	}

	// Remove only known layout entries, never recursively delete unexpected
	// contents. A retry can finish cleanup after the public binary was restored.
	for _, path := range []string{paths.CurrentPath, paths.LastGoodPath, paths.BluePath, paths.GreenPath, layoutDir} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove managed binary path %s: %w", path, err)
		}
	}
	return nil
}
