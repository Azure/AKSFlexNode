package systemd

import (
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

func (d *dbusImpl) writeSystemdFile(
	path string,
	content []byte,
) error {
	// current process runs with root privileges, hence we don't want to
	// overwrite any pre-existing file to avoid unexpected consequences.
	if err := ensureNotOverridingExistingFile(path); err != nil {
		return err
	}

	if err := utilio.WriteFile(path, content, 0600); err != nil {
		return err
	}

	return nil
}

func (d *dbusImpl) WriteUnitFile(
	_ context.Context,
	unitName string,
	content []byte,
) error {
	unitPath := filepath.Join(etcSystemdSystemDir, unitName)

	return d.writeSystemdFile(unitPath, content)
}

func (d *dbusImpl) WriteDropInFile(
	_ context.Context, unitName string, dropInName string, content []byte,
) error {
	return d.writeSystemdFile(
		filepath.Join(etcSystemdSystemDir, unitName+".d", dropInName),
		content,
	)
}

func ensureNotOverridingExistingFile(path string) error {
	_, err := os.Stat(path)
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%q exists, refuse overriding", path)
	}
	return nil
}
