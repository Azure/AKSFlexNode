package systemd

import (
	"context"

	"github.com/coreos/go-systemd/v22/dbus"
)

type dbusImpl struct{}

// New creates a new instance of the systemd Manager.
func New() Manager {
	return &dbusImpl{}
}

var _ Manager = (*dbusImpl)(nil)

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
		if unit.Name == unitName {
			return unit, nil
		}
	}

	return dbus.UnitStatus{}, ErrUnitNotFound
}
