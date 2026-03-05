package systemd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/coreos/go-systemd/v22/dbus"
)

const etcSystemdSystemDir = "/etc/systemd/system"

type dbusImpl struct{}

// New creates a new instance of the systemd Manager.
func New() Manager {
	return &dbusImpl{}
}

var _ Manager = (*dbusImpl)(nil)

func (d *dbusImpl) DaemonReload(ctx context.Context) error {
	conn, err := dbus.NewSystemConnectionContext(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	return conn.ReloadContext(ctx)
}

func drainChan(ctx context.Context, op, unitName string, resultChan <-chan string) error {
	select {
	case result := <-resultChan:
		if result != "done" {
			return fmt.Errorf("unit %q %s: %s", unitName, op, result)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("context cancelled while waiting for unit %q %s: %w", unitName, op, ctx.Err())
	}
}

func (d *dbusImpl) EnableUnit(ctx context.Context, unitName string) error {
	conn, err := dbus.NewSystemConnectionContext(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	_, _, err = conn.EnableUnitFilesContext(ctx, []string{unitName}, false, true)
	if err != nil {
		return err
	}

	return nil
}

func (d *dbusImpl) DisableUnit(ctx context.Context, unitName string) error {
	conn, err := dbus.NewSystemConnectionContext(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	_, err = conn.DisableUnitFilesContext(ctx, []string{unitName}, false)
	if err != nil {
		return err
	}

	return nil
}

func (d *dbusImpl) MaskUnit(ctx context.Context, unitName string) error {
	conn, err := dbus.NewSystemConnectionContext(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	_, err = conn.MaskUnitFilesContext(ctx, []string{unitName}, false, true)
	if err != nil {
		return err
	}

	return nil
}

func (d *dbusImpl) StartUnit(ctx context.Context, unitName string) error {
	conn, err := dbus.NewSystemConnectionContext(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	// StartUnitContext returns a channel that will receive the result of the start operation.
	// We need to wait for the result to ensure the unit is started before we return.
	resultChan := make(chan string, 1)
	if _, err := conn.StartUnitContext(ctx, unitName, "replace", resultChan); err != nil {
		return err
	}
	return drainChan(ctx, "start", unitName, resultChan)
}

func (d *dbusImpl) StopUnit(ctx context.Context, unitName string) error {
	conn, err := dbus.NewSystemConnectionContext(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	resultChan := make(chan string, 1)
	if _, err := conn.StopUnitContext(ctx, unitName, "replace", resultChan); err != nil {
		return err
	}
	return drainChan(ctx, "stop", unitName, resultChan)
}

func (d *dbusImpl) ReloadOrRestartUnit(ctx context.Context, unitName string) error {
	conn, err := dbus.NewSystemConnectionContext(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Try to restart the unit first. If the unit is not running, restart will fail, and we will try to reload it.
	resultChan := make(chan string, 1)
	if _, err := conn.ReloadOrRestartUnitContext(ctx, unitName, "replace", resultChan); err != nil {
		return err
	}

	return drainChan(ctx, "reloadOrRestart", unitName, resultChan)
}

func (d *dbusImpl) GetUnitStatus(ctx context.Context, unitName string) (dbus.UnitStatus, error) {
	conn, err := dbus.NewSystemConnectionContext(ctx)
	if err != nil {
		return dbus.UnitStatus{}, err
	}
	defer conn.Close()

	units, err := conn.ListUnitsByNamesContext(ctx, []string{unitName})
	if err != nil {
		return dbus.UnitStatus{}, err
	}

	for _, unit := range units {
		if unit.LoadState == "not-found" {
			// systemd returns "not-found" for units that don't exist, instead of an error.
			continue
		}

		if unit.Name == unitName {
			return unit, nil
		}
	}

	return dbus.UnitStatus{}, ErrUnitNotFound
}

// ensureSystemdFile idempotently ensures the systemd file at the given path
// has the desired content. It writes only if the content differs from what's
// currently on disk or the file does not exist.
// Returns true if the file was written.
func (d *dbusImpl) ensureSystemdFile(path string, content []byte) (bool, error) {
	currentContent, err := os.ReadFile(path) //#nosec G304 -- trusted path constructed from constant
	switch {
	case errors.Is(err, os.ErrNotExist):
		// File doesn't exist, fall through to write it
	case err != nil:
		return false, err
	default:
		if bytes.Equal(bytes.TrimSpace(currentContent), bytes.TrimSpace(content)) {
			return false, nil
		}
	}

	if err := utilio.WriteFile(path, content, 0600); err != nil {
		return false, err
	}
	return true, nil
}

func (d *dbusImpl) EnsureUnitFile(
	_ context.Context,
	unitName string,
	content []byte,
) (bool, error) {
	unitPath := filepath.Join(etcSystemdSystemDir, unitName)
	return d.ensureSystemdFile(unitPath, content)
}

func (d *dbusImpl) EnsureDropInFile(
	_ context.Context,
	unitName string,
	dropInName string,
	content []byte,
) (bool, error) {
	dropInPath := filepath.Join(etcSystemdSystemDir, unitName+".d", dropInName)
	return d.ensureSystemdFile(dropInPath, content)
}
