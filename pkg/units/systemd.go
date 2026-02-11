package units

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/coreos/go-systemd/v22/dbus"
)

// Manager abstracts systemd daemon control so that the real D-Bus
// implementation can be swapped for a fake in tests or early development.
type Manager interface {
	// ReloadDaemon asks systemd to reload its configuration (daemon-reload).
	ReloadDaemon() error

	// StartUnit starts a unit by name.
	StartUnit(name string) error

	// RestartUnit restarts a unit by name.
	RestartUnit(name string) error

	// ReloadUnit reloads a unit by name (e.g. SIGHUP).
	ReloadUnit(name string) error

	// StopUnit stops a unit by name.
	StopUnit(name string) error

	// Close releases any resources held by the manager (e.g. D-Bus
	// connections). It is safe to call multiple times. Implementations
	// that hold no resources may treat this as a no-op.
	Close()
}

// dbusManager is the production implementation of Manager that talks to
// systemd over D-Bus using github.com/coreos/go-systemd/v22/dbus.
type dbusManager struct {
	conn *dbus.Conn
}

var _ Manager = (*dbusManager)(nil)

// NewManager creates a new Manager backed by a real D-Bus connection to
// the system's systemd instance.
//
// The caller should call Close on the returned Manager when it is no
// longer needed. If the Manager does not need closing (e.g. a fake),
// Close is a no-op.
func NewManager(ctx context.Context) (Manager, error) {
	conn, err := dbus.NewWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("connecting to systemd via D-Bus: %w", err)
	}
	return &dbusManager{conn: conn}, nil
}

// Close closes the underlying D-Bus connection. It is safe to call
// multiple times.
func (m *dbusManager) Close() {
	if m.conn != nil {
		m.conn.Close()
	}
}

// ReloadDaemon instructs systemd to re-scan and reload all unit files,
// equivalent to `systemctl daemon-reload`.
func (m *dbusManager) ReloadDaemon() error {
	log.Printf("[systemd] daemon-reload")
	if err := m.conn.ReloadContext(context.TODO()); err != nil {
		return fmt.Errorf("daemon-reload: %w", err)
	}
	return nil
}

// StartUnit starts the named unit, waiting for the job to complete.
// The mode is "replace", which is equivalent to `systemctl start <name>`.
func (m *dbusManager) StartUnit(name string) error {
	log.Printf("[systemd] start %s", name)
	return m.doUnit("start", name, func(ctx context.Context, n, mode string, ch chan<- string) (int, error) {
		return m.conn.StartUnitContext(ctx, n, mode, ch)
	})
}

// RestartUnit restarts the named unit, waiting for the job to complete.
// The mode is "replace", which is equivalent to `systemctl restart <name>`.
func (m *dbusManager) RestartUnit(name string) error {
	log.Printf("[systemd] restart %s", name)
	return m.doUnit("restart", name, func(ctx context.Context, n, mode string, ch chan<- string) (int, error) {
		return m.conn.RestartUnitContext(ctx, n, mode, ch)
	})
}

// ReloadUnit reloads the named unit (e.g. sends SIGHUP), waiting for the
// job to complete. The mode is "replace", equivalent to
// `systemctl reload <name>`.
func (m *dbusManager) ReloadUnit(name string) error {
	log.Printf("[systemd] reload %s", name)
	return m.doUnit("reload", name, func(ctx context.Context, n, mode string, ch chan<- string) (int, error) {
		return m.conn.ReloadUnitContext(ctx, n, mode, ch)
	})
}

// StopUnit stops the named unit, waiting for the job to complete.
// The mode is "replace", which is equivalent to `systemctl stop <name>`.
func (m *dbusManager) StopUnit(name string) error {
	log.Printf("[systemd] stop %s", name)
	return m.doUnit("stop", name, func(ctx context.Context, n, mode string, ch chan<- string) (int, error) {
		return m.conn.StopUnitContext(ctx, n, mode, ch)
	})
}

// unitFunc is the signature shared by Start/Stop/Restart/ReloadUnitContext.
type unitFunc func(ctx context.Context, name, mode string, ch chan<- string) (int, error)

// doUnit is the common implementation for unit operations. It enqueues
// the job with mode "replace" and waits synchronously for the job result
// via a channel. A result of "done" is considered success; anything else
// (canceled, timeout, failed, dependency, skipped) is treated as an error.
func (m *dbusManager) doUnit(verb, name string, fn unitFunc) error {
	ch := make(chan string, 1)
	ctx := context.TODO()

	_, err := fn(ctx, name, "replace", ch)
	if err != nil {
		return fmt.Errorf("%s %s: %w", verb, name, err)
	}

	result := <-ch
	if result != "done" {
		return fmt.Errorf("%s %s: job result %q", verb, name, result)
	}

	return nil
}

// FakeManager is a development/test implementation that logs actions
// instead of talking to a real systemd instance.
type FakeManager struct {
	// Actions records every operation as "verb:unit" strings for test assertions.
	Actions []string
}

var _ Manager = (*FakeManager)(nil)

// Close is a no-op for the fake manager.
func (f *FakeManager) Close() {}

func (f *FakeManager) record(action, unit string) {
	entry := fmt.Sprintf("%s:%s", action, unit)
	f.Actions = append(f.Actions, entry)
	log.Printf("[systemd-fake] %s %s", action, unit)
}

// ReloadDaemon records a daemon-reload action.
func (f *FakeManager) ReloadDaemon() error {
	f.Actions = append(f.Actions, "daemon-reload")
	log.Printf("[systemd-fake] daemon-reload")
	return nil
}

// StartUnit records a start action.
func (f *FakeManager) StartUnit(name string) error {
	f.record("start", name)
	return nil
}

// RestartUnit records a restart action.
func (f *FakeManager) RestartUnit(name string) error {
	f.record("restart", name)
	return nil
}

// ReloadUnit records a reload action.
func (f *FakeManager) ReloadUnit(name string) error {
	f.record("reload", name)
	return nil
}

// StopUnit records a stop action.
func (f *FakeManager) StopUnit(name string) error {
	f.record("stop", name)
	return nil
}

// systemdUnitDeltas describes the set of actions needed to transition from
// one generation of systemd units to the next, inspired by NixOS's
// switch-to-configuration.
//
// The delta categories are:
//   - UnitsToStart: units that exist in the new generation but not the old
//   - UnitsToStop: units that exist in the old generation but not the new
//   - UnitsToRestart: units that exist in both but whose content changed
//   - UnitsToReload: units whose changes can be applied via reload (e.g. Mount Options)
type systemdUnitDeltas struct {
	UnitsToStart   []string
	UnitsToStop    []string
	UnitsToRestart []string
	UnitsToReload  []string
}

// generateDeltas compares two directories of systemd unit files (the old
// generation and the new generation) and computes the minimal set of
// start/stop/restart actions needed.
//
// The algorithm mirrors the core logic of NixOS's switch-to-configuration:
//  1. Units only in oldPath → stop
//  2. Units only in newPath → start
//  3. Units in both with different content → restart
//  4. Units in both with identical content → no action
//
// Both directories should contain flat files named <unit>.service (or other
// systemd unit suffixes). Subdirectories are ignored.
//
// Either oldPath or newPath may be empty strings, meaning "no previous/new
// generation". An empty oldPath means everything is new (all start). An
// empty newPath means everything should stop.
func generateDeltas(oldPath, newPath string) (*systemdUnitDeltas, error) {
	oldUnits, err := readUnitDir(oldPath)
	if err != nil {
		return nil, fmt.Errorf("reading old unit dir %q: %w", oldPath, err)
	}

	newUnits, err := readUnitDir(newPath)
	if err != nil {
		return nil, fmt.Errorf("reading new unit dir %q: %w", newPath, err)
	}

	return computeDeltas(oldUnits, newUnits)
}

// computeDeltas computes systemd unit deltas from two pre-read unit maps.
// This is the core algorithm shared by generateDeltas (which reads from
// directories) and Overlay.Apply (which uses walkUnitDir for symlink trees).
//
// Unlike a naive byte-level comparison, this performs semantic comparison of
// unit files following rules inspired by NixOS switch-to-configuration-ng:
//   - The [Install] section is ignored (only relevant at enable-time).
//   - Certain [Unit] metadata keys (Description, Documentation, etc.) are ignored.
//   - Changes to Options in [Mount] trigger reload, not restart.
//   - All other meaningful differences trigger restart.
func computeDeltas(oldUnits, newUnits map[string][]byte) (*systemdUnitDeltas, error) {
	deltas := &systemdUnitDeltas{}

	// Units in old but not new → stop.
	for name := range oldUnits {
		if _, ok := newUnits[name]; !ok {
			deltas.UnitsToStop = append(deltas.UnitsToStop, name)
		}
	}

	// Units in new but not old → start.
	// Units in both → semantic comparison.
	for name, newContent := range newUnits {
		oldContent, exists := oldUnits[name]
		if !exists {
			deltas.UnitsToStart = append(deltas.UnitsToStart, name)
			continue
		}

		cmp, err := compareUnitContents(oldContent, newContent)
		if err != nil {
			return nil, fmt.Errorf("comparing unit %q: %w", name, err)
		}

		switch cmp {
		case UnitNeedsRestart:
			deltas.UnitsToRestart = append(deltas.UnitsToRestart, name)
		case UnitNeedsReload:
			deltas.UnitsToReload = append(deltas.UnitsToReload, name)
		case UnitEqual:
			// No action needed.
		}
	}

	// Sort all slices for deterministic output.
	sort.Strings(deltas.UnitsToStart)
	sort.Strings(deltas.UnitsToStop)
	sort.Strings(deltas.UnitsToRestart)
	sort.Strings(deltas.UnitsToReload)

	return deltas, nil
}

// readUnitDir reads all regular files (and symlink targets) from a directory
// and returns a map of filename → content. Returns an empty map if path is
// empty or does not exist.
func readUnitDir(path string) (map[string][]byte, error) {
	units := make(map[string][]byte)
	if path == "" {
		return units, nil
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		if os.IsNotExist(err) {
			return units, nil
		}
		return nil, err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		// Accept only known systemd unit suffixes.
		name := entry.Name()
		if !isSystemdUnitFile(name) {
			continue
		}

		content, err := os.ReadFile(filepath.Join(path, name))
		if err != nil {
			return nil, fmt.Errorf("reading unit file %q: %w", name, err)
		}
		units[name] = content
	}

	return units, nil
}

// isSystemdUnitFile reports whether the filename has a recognized systemd
// unit suffix.
func isSystemdUnitFile(name string) bool {
	suffixes := []string{
		".service", ".socket", ".device", ".mount", ".automount",
		".swap", ".target", ".path", ".timer", ".slice", ".scope",
	}
	for _, s := range suffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

// applyDeltas executes the computed deltas against a Manager:
// stop removed units, daemon-reload, then start new and restart changed.
func applyDeltas(mgr Manager, deltas *systemdUnitDeltas) error {
	// Phase 1: Stop units that were removed.
	for _, unit := range deltas.UnitsToStop {
		if err := mgr.StopUnit(unit); err != nil {
			return fmt.Errorf("stopping unit %q: %w", unit, err)
		}
	}

	// Phase 2: Daemon-reload so systemd picks up new/changed unit files.
	if err := mgr.ReloadDaemon(); err != nil {
		return fmt.Errorf("reloading systemd daemon: %w", err)
	}

	// Phase 3: Start new units.
	for _, unit := range deltas.UnitsToStart {
		if err := mgr.StartUnit(unit); err != nil {
			return fmt.Errorf("starting unit %q: %w", unit, err)
		}
	}

	// Phase 4: Restart changed units.
	for _, unit := range deltas.UnitsToRestart {
		if err := mgr.RestartUnit(unit); err != nil {
			return fmt.Errorf("restarting unit %q: %w", unit, err)
		}
	}

	// Phase 5: Reload units.
	for _, unit := range deltas.UnitsToReload {
		if err := mgr.ReloadUnit(unit); err != nil {
			return fmt.Errorf("reloading unit %q: %w", unit, err)
		}
	}

	return nil
}

// resolveUnitDir resolves a systemd unit directory from an etc overlay's
// installed state path. The convention is:
//
//	<statePath>/etc/systemd/system/
func resolveUnitDir(etcOverlay *InstalledPackage) string {
	if etcOverlay == nil {
		return ""
	}
	return filepath.Join(etcOverlay.InstalledStatePath, "etc", "systemd", "system")
}

// walkUnitDir is a variant of readUnitDir that works through symlink chains,
// which is important because the etc overlay tree is composed of symlinks.
// It resolves symlinks before reading, so the content comparison is against
// the actual file content the symlinks point to.
func walkUnitDir(path string) (map[string][]byte, error) {
	units := make(map[string][]byte)
	if path == "" {
		return units, nil
	}

	// Resolve the path itself in case it's a symlink.
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		if os.IsNotExist(err) {
			return units, nil
		}
		return nil, err
	}

	err = filepath.WalkDir(resolved, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != resolved {
				return filepath.SkipDir // don't recurse into subdirs
			}
			return nil
		}
		name := d.Name()
		if !isSystemdUnitFile(name) {
			return nil
		}
		content, readErr := os.ReadFile(p)
		if readErr != nil {
			return fmt.Errorf("reading unit file %q: %w", name, readErr)
		}
		units[name] = content
		return nil
	})
	if err != nil {
		return nil, err
	}

	return units, nil
}
