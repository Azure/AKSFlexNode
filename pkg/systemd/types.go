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
	// DaemonReload triggers a systemd daemon reload to recognize new or changed unit files.
	DaemonReload(ctx context.Context) error

	// EnableUnit enables a systemd unit by name, allowing it to start on boot.
	EnableUnit(ctx context.Context, unitName string) error
	// StartUnit starts a systemd unit by name.
	StartUnit(ctx context.Context, unitName string) error
	// ReloadOrRestartUnit reloads or restarts a systemd unit by name.
	ReloadOrRestartUnit(ctx context.Context, unitName string) error

	// GetUnitStatus retrieves the status of a systemd unit by name.
	// Returns ErrUnitNotFound if no unit with the specified name exists.
	GetUnitStatus(ctx context.Context, unitName string) (dbus.UnitStatus, error)

	// WriteUnitFile writes a systemd unit file with the given name and content.
	WriteUnitFile(ctx context.Context, unitName string, content []byte) error
	// WriteDropInFile writes a systemd drop-in file for the specified unit.
	WriteDropInFile(ctx context.Context, unitName string, dropInName string, content []byte) error
}
