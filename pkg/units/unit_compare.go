package units

import (
	"bytes"
	"fmt"

	sdunit "github.com/coreos/go-systemd/v22/unit"
)

// unitInfo is a parsed systemd unit file grouped as:
//
//	section name → key → list of values
//
// Multi-valued keys (like ExecStartPre=) accumulate values in the slice.
// An empty value (e.g. "ExecStart=") resets all previous values for that key,
// following systemd's override semantics.
type unitInfo = map[string]map[string][]string

// UnitComparison describes the result of comparing two unit files.
type UnitComparison int

const (
	// UnitEqual means the two unit files are semantically identical
	// (ignoring the [Install] section and certain [Unit] metadata keys).
	UnitEqual UnitComparison = iota

	// UnitNeedsRestart means the units differ in a way that requires
	// stopping and restarting the service.
	UnitNeedsRestart

	// UnitNeedsReload means the only differences are in fields that
	// can be applied via a reload (e.g. Options in [Mount]).
	UnitNeedsReload
)

// unitSectionIgnores is the set of [Unit] section keys whose changes are
// ignored during comparison (they don't trigger restart or reload).
//
// This matches the NixOS switch-to-configuration-ng behavior, except we
// do NOT support X-Reload-Triggers (dropped for simplicity).
var unitSectionIgnores = map[string]struct{}{
	"Description":       {},
	"Documentation":     {},
	"OnFailure":         {},
	"OnSuccess":         {},
	"OnFailureJobMode":  {},
	"IgnoreOnIsolate":   {},
	"StopWhenUnneeded":  {},
	"RefuseManualStart": {},
	"RefuseManualStop":  {},
	"AllowIsolate":      {},
	"CollectMode":       {},
	"SourcePath":        {},
}

// parseSystemdINI parses a systemd unit file (INI-like format) into a UnitInfo
// structure using github.com/coreos/go-systemd/v22/unit for low-level parsing.
//
// On top of the library's parsing, this applies the following semantics:
//
//   - The [Install] section is skipped entirely (it has no runtime relevance).
//   - Multi-valued keys accumulate in order.
//   - An empty value (e.g. "ExecStart=") clears all existing values for that key,
//     following systemd's override semantics.
//
// The function takes a mutable UnitInfo so it can be called multiple times for
// drop-in override files that extend the base unit.
func parseSystemdINI(data unitInfo, content []byte) error {
	opts, err := sdunit.DeserializeOptions(bytes.NewReader(content))
	if err != nil {
		return fmt.Errorf("deserializing systemd unit options: %w", err)
	}

	for _, opt := range opts {
		// Skip the [Install] section (only relevant at enable-time).
		if opt.Section == "Install" {
			continue
		}

		// Ensure section map exists.
		sectionMap, ok := data[opt.Section]
		if !ok {
			sectionMap = make(map[string][]string)
			data[opt.Section] = sectionMap
		}

		// Empty value means "clear all previous values for this key"
		// (systemd override semantics).
		if opt.Value == "" {
			sectionMap[opt.Name] = nil
		} else {
			sectionMap[opt.Name] = append(sectionMap[opt.Name], opt.Value)
		}
	}

	return nil
}

// parseUnitContent parses a systemd unit file from raw bytes into a unitInfo.
func parseUnitContent(content []byte) (unitInfo, error) {
	info := make(unitInfo)
	if err := parseSystemdINI(info, content); err != nil {
		return nil, err
	}
	return info, nil
}

// compareUnits compares two parsed systemd unit files and determines whether
// the unit needs to be restarted, reloaded, or is unchanged. This implements
// logic inspired by NixOS switch-to-configuration-ng's compare_units function.
//
// Rules:
//   - The [Install] section is stripped during parsing and never compared.
//   - The [Unit] section is treated specially: most metadata keys are ignored.
//   - Changes to [Mount] Options trigger reload instead of restart.
//   - All other differences trigger a restart.
//   - If the only differences are reload-worthy, UnitNeedsReload is returned.
//   - If no differences exist, UnitEqual is returned.
func compareUnits(current, new unitInfo) UnitComparison {
	ret := UnitEqual

	// Track which sections in the new unit we've visited.
	newSectionsSeen := make(map[string]bool)

	// Phase 1: Iterate over sections in the current unit.
	for sectionName, currentSection := range current {
		newSection, existsInNew := new[sectionName]

		if !existsInNew {
			// Section in current but missing from new.
			if sectionName == "Unit" {
				// Only OK if all keys in the old [Unit] section are ignorable.
				for key := range currentSection {
					if _, ignored := unitSectionIgnores[key]; !ignored {
						return UnitNeedsRestart
					}
				}
				continue
			}
			return UnitNeedsRestart
		}

		newSectionsSeen[sectionName] = true

		// Track which keys in the new section we've visited.
		newKeysSeen := make(map[string]bool)

		// Compare keys in current section.
		for key, currentValues := range currentSection {
			newValues, keyExistsInNew := newSection[key]
			newKeysSeen[key] = true

			if !keyExistsInNew {
				// Key removed in new unit.
				if sectionName == "Unit" {
					if _, ignored := unitSectionIgnores[key]; ignored {
						continue
					}
				}
				return UnitNeedsRestart
			}

			// Compare values.
			if !stringSlicesEqual(currentValues, newValues) {
				if sectionName == "Unit" {
					if _, ignored := unitSectionIgnores[key]; ignored {
						continue
					}
				}
				if sectionName == "Mount" && key == "Options" {
					ret = UnitNeedsReload
					continue
				}
				return UnitNeedsRestart
			}
		}

		// Check for keys in new section that weren't in current.
		for key := range newSection {
			if newKeysSeen[key] {
				continue
			}
			// New key introduced.
			if sectionName == "Unit" {
				if _, ignored := unitSectionIgnores[key]; ignored {
					continue
				}
				return UnitNeedsRestart
			} else {
				return UnitNeedsRestart
			}
		}
	}

	// Phase 2: Check for sections in new that weren't in current.
	for sectionName := range new {
		if newSectionsSeen[sectionName] {
			continue
		}
		// New section introduced.
		if sectionName == "Unit" {
			// Only OK if all new keys are ignorable.
			for key := range new[sectionName] {
				if _, ignored := unitSectionIgnores[key]; !ignored {
					return UnitNeedsRestart
				}
			}
		} else {
			return UnitNeedsRestart
		}
	}

	return ret
}

// compareUnitContents is a convenience function that parses two unit file
// byte contents and compares them semantically.
func compareUnitContents(oldContent, newContent []byte) (UnitComparison, error) {
	oldInfo, err := parseUnitContent(oldContent)
	if err != nil {
		return UnitEqual, fmt.Errorf("parsing old unit content: %w", err)
	}
	newInfo, err := parseUnitContent(newContent)
	if err != nil {
		return UnitEqual, fmt.Errorf("parsing new unit content: %w", err)
	}
	return compareUnits(oldInfo, newInfo), nil
}

// stringSlicesEqual reports whether two string slices are equal.
func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
