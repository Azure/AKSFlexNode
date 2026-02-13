package systemd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/coreos/go-systemd/v22/dbus"
	"github.com/google/renameio/v2"
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

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	if err := renameio.WriteFile(path, content, 0600); err != nil {
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
