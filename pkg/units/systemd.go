package units

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// systemdManager abstracts systemd daemon control so that the real D-Bus
// implementation can be swapped for a fake in tests or early development.
type systemdManager interface {
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

// applyDeltas executes the computed deltas against a systemdManager:
// stop removed units, daemon-reload, then start new and restart changed.
func applyDeltas(mgr systemdManager, deltas *systemdUnitDeltas) error {
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

// fakeSystemdManager is a development/test implementation that logs actions
// instead of talking to a real systemd instance.
type fakeSystemdManager struct {
	// Actions records every operation as "verb:unit" strings for test assertions.
	Actions []string
}

var _ systemdManager = (*fakeSystemdManager)(nil)

func (f *fakeSystemdManager) record(action, unit string) {
	entry := fmt.Sprintf("%s:%s", action, unit)
	f.Actions = append(f.Actions, entry)
	log.Printf("[systemd-fake] %s %s", action, unit)
}

func (f *fakeSystemdManager) ReloadDaemon() error {
	f.Actions = append(f.Actions, "daemon-reload")
	log.Printf("[systemd-fake] daemon-reload")
	return nil
}

func (f *fakeSystemdManager) StartUnit(name string) error {
	f.record("start", name)
	return nil
}

func (f *fakeSystemdManager) RestartUnit(name string) error {
	f.record("restart", name)
	return nil
}

func (f *fakeSystemdManager) ReloadUnit(name string) error {
	f.record("reload", name)
	return nil
}

func (f *fakeSystemdManager) StopUnit(name string) error {
	f.record("stop", name)
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
