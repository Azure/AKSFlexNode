package systemd

import (
	"context"
	"errors"

	"github.com/coreos/go-systemd/v22/dbus"
)

const (
	UnitActiveStateActive   = "active"
	UnitActiveStateInactive = "inactive"
	UnitActiveStateFailed   = "failed"
)

var ErrUnitNotFound = errors.New("unit not found")

// Manager defines the interface for interacting with systemd.
type Manager interface {
	// GetUnitStatus retrieves the status of a systemd unit by name.
	// Returns ErrUnitNotFound if no unit with the specified name exists.
	GetUnitStatus(ctx context.Context, unitName string) (dbus.UnitStatus, error)
}
