package systemd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// EnsureUnitRunning ensures a systemd unit is active.
//
// When reloadDaemon is true, the systemd daemon is reloaded first so that it
// picks up any unit file changes on disk. This must be set whenever the unit
// file (or a drop-in) has been created or modified.
//
// When restartUnit is true and the unit is already active, it is
// reloaded/restarted (e.g. because associated configuration changed).
//
// If the unit is not active regardless of the flags, it is started.
func EnsureUnitRunning(
	ctx context.Context,
	m Manager,
	unitName string,
	reloadDaemon bool,
	restartUnit bool,
) error {
	if reloadDaemon {
		if err := m.DaemonReload(ctx); err != nil {
			return err
		}
	}

	status, err := m.GetUnitStatus(ctx, unitName)
	switch {
	case errors.Is(err, ErrUnitNotFound):
		return m.StartUnit(ctx, unitName)
	case err != nil:
		return err
	default:
		if status.ActiveState != UnitActiveStateActive {
			return m.StartUnit(ctx, unitName)
		}
		if reloadDaemon || restartUnit {
			return m.ReloadOrRestartUnit(ctx, unitName)
		}
		return nil
	}
}

// EnsureUnitMasked idempotently stops, disables, and masks a systemd unit.
// It is a no-op if the unit does not exist.
func EnsureUnitMasked(
	ctx context.Context,
	m Manager,
	unitName string,
) error {
	status, err := m.GetUnitStatus(ctx, unitName)
	switch {
	case errors.Is(err, ErrUnitNotFound):
		// unit does not exist, nothing to do
		return nil
	case err != nil:
		return err
	default:
		if status.ActiveState == UnitActiveStateActive {
			if err := m.StopUnit(ctx, unitName); err != nil {
				return err
			}
		}
	}

	if err := m.DisableUnit(ctx, unitName); err != nil {
		return err
	}

	// Remove the unit file and drop-in directory so MaskUnit can create
	// the /dev/null symlink. The dbus MaskUnitFiles "force" flag only
	// replaces existing symlinks, not regular files, so a unit file
	// written by a prior operation (e.g. EnsureUnitFile) would block it.
	unitPath := filepath.Join(etcSystemdSystemDir, unitName)
	if err := os.Remove(unitPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove unit file before mask: %w", err)
	}
	if err := os.RemoveAll(unitPath + ".d"); err != nil {
		return fmt.Errorf("remove drop-in dir before mask: %w", err)
	}

	return m.MaskUnit(ctx, unitName)
}

// EnsureUnitStoppedAndDisabled idempotently stops (if active) and disables a systemd unit.
// It is a no-op if the unit does not exist.
func EnsureUnitStoppedAndDisabled(
	ctx context.Context,
	m Manager,
	unitName string,
) error {
	status, err := m.GetUnitStatus(ctx, unitName)
	switch {
	case errors.Is(err, ErrUnitNotFound):
		// unit does not exist, nothing to do
		return nil
	case err != nil:
		return err
	default:
		if status.ActiveState == UnitActiveStateActive {
			if err := m.StopUnit(ctx, unitName); err != nil {
				return err
			}
		}
	}

	return m.DisableUnit(ctx, unitName)
}
