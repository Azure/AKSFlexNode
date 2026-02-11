package units

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

const (
	// etcStaticName is the name of the symlink under <rootDir>/etc that
	// points to the current etc overlay's etc/ tree. Analogous to NixOS's
	// /etc/static link.
	etcStaticName = "static"
)

type etcManager struct {
	rootDir   string
	statesDir string // store states directory (e.g. /aks-flex/states); symlinks pointing here are managed by us
}

func newEtcManager(rootDir, statesDir string) *etcManager {
	return &etcManager{rootDir: rootDir, statesDir: statesDir}
}

// etcDir returns the absolute path to <rootDir>/etc.
func (e *etcManager) etcDir() string {
	return filepath.Join(e.rootDir, "etc")
}

// staticPath returns the absolute path to <rootDir>/etc/static.
func (e *etcManager) staticPath() string {
	return filepath.Join(e.etcDir(), etcStaticName)
}

// symlinkToStatic creates (or atomically replaces) the symlink at
// <rootDir>/etc/static so it points to source, which should be the
// etc overlay's <statePath>/etc directory containing the unified
// symlink tree.
//
// The update is as atomic as the filesystem allows: create a temporary
// symlink next to the target, then os.Rename over the old one.
func (e *etcManager) symlinkToStatic(source string) error {
	etcDir := e.etcDir()
	if err := os.MkdirAll(etcDir, dirPermissions); err != nil {
		return fmt.Errorf("creating etc dir %s: %w", etcDir, err)
	}

	staticLink := e.staticPath()

	// Create a temporary symlink next to the target so we can atomically
	// rename it. os.Rename replaces the destination on POSIX systems.
	tmpLink, err := os.CreateTemp(etcDir, ".static-tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp file for static symlink: %w", err)
	}
	tmpPath := tmpLink.Name()
	tmpLink.Close()
	os.Remove(tmpPath) // need the name, not the file

	if err := os.Symlink(source, tmpPath); err != nil {
		return fmt.Errorf("creating temp symlink %s -> %s: %w", tmpPath, source, err)
	}

	if err := os.Rename(tmpPath, staticLink); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("atomically replacing static symlink: %w", err)
	}

	return nil
}

// promoteStaticToEtc walks the tree under <rootDir>/etc/static, and for
// every file (or symlink leaf) found, creates a corresponding symlink at
// <rootDir>/etc/<relative> pointing directly into the store directory
// that /etc/static references (read via Readlink), rather than through
// the /etc/static symlink itself.
//
// After creating all new symlinks it removes stale entries: symlinks in
// /etc that point into the store's states directory but are NOT in the
// current generation set.
//
// If a non-symlink regular file already exists at a target path in /etc,
// it is skipped with an error collected but does not abort the walk —
// we never silently overwrite files we don't manage.
func (e *etcManager) promoteStaticToEtc() error {
	staticDir := e.staticPath()

	// Read the direct target of /etc/static (one level, no chain resolution).
	// This is the store path we want /etc entries to point into.
	staticTarget, err := os.Readlink(staticDir)
	if err != nil {
		return fmt.Errorf("reading static symlink %s: %w", staticDir, err)
	}

	// Resolve the static symlink fully to get the real directory to walk
	// (EvalSymlinks follows the entire chain).
	resolvedStatic, err := filepath.EvalSymlinks(staticDir)
	if err != nil {
		return fmt.Errorf("resolving static symlink %s: %w", staticDir, err)
	}

	// Walk the resolved static tree to discover all leaf entries and
	// create/update their /etc symlinks.
	newTargets := make(map[string]struct{})
	var promoteErrors []error

	err = filepath.WalkDir(resolvedStatic, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Skip directories — we only symlink leaf entries.
		if d.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(resolvedStatic, path)
		if err != nil {
			return fmt.Errorf("computing relative path for %s: %w", path, err)
		}

		if err := e.promoteEntry(rel, staticTarget); err != nil {
			promoteErrors = append(promoteErrors, err)
			return nil // continue walking
		}

		newTargets[rel] = struct{}{}
		return nil
	})
	if err != nil {
		return fmt.Errorf("walking static dir %s: %w", resolvedStatic, err)
	}

	// Clean up stale symlinks from previous generations.
	e.cleanupStaleSymlinks(newTargets)

	if len(promoteErrors) > 0 {
		return fmt.Errorf("encountered %d errors promoting static entries (first: %w)", len(promoteErrors), promoteErrors[0])
	}

	return nil
}

// Apply activates a new etc generation by pointing <rootDir>/etc/static at
// source and then promoting all entries into /etc as symlinks.
func (e *etcManager) Apply(source string) error {
	if err := e.symlinkToStatic(source); err != nil {
		return fmt.Errorf("creating static symlink: %w", err)
	}
	if err := e.promoteStaticToEtc(); err != nil {
		return fmt.Errorf("promoting static entries to etc: %w", err)
	}
	return nil
}

// promoteEntry creates a single symlink at <rootDir>/etc/<target>
// pointing to <staticTarget>/<target>, where staticTarget is the direct
// Readlink of /etc/static (the store path). This means the /etc symlink
// points directly into the store, bypassing the /etc/static indirection.
func (e *etcManager) promoteEntry(target, staticTarget string) error {
	linkPath := filepath.Join(e.etcDir(), target)
	linkTarget := filepath.Join(staticTarget, target)

	// Check if the symlink already exists and is correct.
	existing, err := os.Readlink(linkPath)
	if err == nil && existing == linkTarget {
		return nil // already correct
	}

	// If something exists at linkPath, check if it's a non-symlink.
	info, statErr := os.Lstat(linkPath)
	if statErr == nil && info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("refusing to overwrite non-symlink file at %s", linkPath)
	}

	// Create parent directories (e.g. for "systemd/system/foo.service").
	if err := os.MkdirAll(filepath.Dir(linkPath), dirPermissions); err != nil {
		return fmt.Errorf("creating parent directory for %s: %w", target, err)
	}

	// Atomic replace: temp symlink + rename.
	tmpLink, err := os.CreateTemp(filepath.Dir(linkPath), ".promote-tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp file for promote: %w", err)
	}
	tmpPath := tmpLink.Name()
	tmpLink.Close()
	os.Remove(tmpPath)

	if err := os.Symlink(linkTarget, tmpPath); err != nil {
		return fmt.Errorf("creating temp symlink for %s: %w", target, err)
	}

	if err := os.Rename(tmpPath, linkPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("atomically replacing symlink for %s: %w", target, err)
	}

	return nil
}

// cleanupStaleSymlinks walks <rootDir>/etc and removes any symlinks that
// point into the store's states directory but whose relative target is
// NOT in the newTargets set — meaning they are leftovers from a previous
// generation. Empty parent directories are cleaned up afterward.
func (e *etcManager) cleanupStaleSymlinks(newTargets map[string]struct{}) {
	etcDir := e.etcDir()
	staticPrefix := e.staticPath()

	filepath.WalkDir(etcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // best-effort
		}

		// Skip the static symlink itself. Since static is a symlink (not
		// a directory), WalkDir won't descend into it, so we just skip
		// this entry. (Returning SkipDir on a non-directory would skip
		// the remaining siblings.)
		if path == staticPrefix {
			return nil
		}

		// Only inspect symlinks.
		if d.Type()&os.ModeSymlink == 0 {
			return nil
		}

		dest, err := os.Readlink(path)
		if err != nil {
			return nil
		}

		// Only consider symlinks that point into the store's states dir.
		if !isUnderDir(dest, e.statesDir) {
			return nil
		}

		// Compute the relative path this entry represents under /etc.
		rel, err := filepath.Rel(etcDir, path)
		if err != nil {
			return nil
		}

		// If this relative path is in the current generation, keep it.
		if _, ok := newTargets[rel]; ok {
			return nil
		}

		// Stale: remove the symlink and clean up empty parents.
		os.Remove(path)
		e.cleanupEmptyParents(path)
		return nil
	})
}

// cleanupEmptyParents removes empty parent directories up to (but not
// including) the etc dir.
func (e *etcManager) cleanupEmptyParents(path string) {
	etcDir := e.etcDir()
	dir := filepath.Dir(path)
	for dir != etcDir && dir != "." && dir != "/" {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			break
		}
		os.Remove(dir)
		dir = filepath.Dir(dir)
	}
}

// isUnderDir reports whether path is strictly under dir.
func isUnderDir(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return len(rel) > 0 && rel[0] != '.'
}

// CurrentStaticTarget reads the current /etc/static symlink and returns
// the directory it points to, or "" if the symlink does not exist or cannot
// be read. This is used to discover the previous generation's etc tree
// before replacing it.
func (e *etcManager) CurrentStaticTarget() string {
	target, err := os.Readlink(e.staticPath())
	if err != nil {
		return ""
	}
	return target
}
