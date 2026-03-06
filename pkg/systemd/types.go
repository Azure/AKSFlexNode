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
	// DisableUnit disables a systemd unit by name, preventing it from starting on boot.
	DisableUnit(ctx context.Context, unitName string) error
	// MaskUnit masks a systemd unit, preventing it from being started by any means.
	MaskUnit(ctx context.Context, unitName string) error

	// StartUnit starts a systemd unit by name.
	StartUnit(ctx context.Context, unitName string) error
	// StopUnit stops a systemd unit by name.
	StopUnit(ctx context.Context, unitName string) error
	// ReloadOrRestartUnit reloads or restarts a systemd unit by name.
	ReloadOrRestartUnit(ctx context.Context, unitName string) error

	// GetUnitStatus retrieves the status of a systemd unit by name.
	// Returns ErrUnitNotFound if no unit with the specified name exists.
	GetUnitStatus(ctx context.Context, unitName string) (dbus.UnitStatus, error)

	// EnsureUnitFile idempotently ensures the systemd unit file in /etc/systemd/system/
	// has the desired content. It writes the file only if the content differs from
	// what's currently on disk. This is useful when overriding a vendor-provided unit
	// file in /lib/systemd/system/ with a custom one.
	// Returns true if the file was written (i.e., content changed or file was created).
	EnsureUnitFile(ctx context.Context, unitName string, content []byte) (bool, error)
	// EnsureDropInFile idempotently ensures a systemd drop-in file for the specified unit
	// has the desired content. It writes the file only if the content differs from
	// what's currently on disk.
	// Returns true if the file was written (i.e., content changed or file was created).
	EnsureDropInFile(ctx context.Context, unitName string, dropInName string, content []byte) (bool, error)
}
