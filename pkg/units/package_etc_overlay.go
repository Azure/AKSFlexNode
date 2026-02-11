package units

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// etcOverlayPackage implements Package. It collects all declared EtcFiles from
// a set of installed packages and creates a single "etc" state directory
// containing symlinks that point back into each package's state dir.
//
// This is analogous to NixOS's /nix/store/<hash>-etc/etc — a unified etc tree
// built from all package contributions, suitable for atomically switching the
// system's /etc to point at this overlay.
//
// Layout produced by Install:
//
//	<base>/
//	  |- etc/
//	       |- <target-path> -> <package-state-dir>/<source-path>
//	       |- ...
type etcOverlayPackage struct {
	version  string
	packages []*InstalledPackage
}

func newEtcOverlayPackage(version string, packages []*InstalledPackage) *etcOverlayPackage {
	return &etcOverlayPackage{
		version:  version,
		packages: packages,
	}
}

var _ Package = (*etcOverlayPackage)(nil)

func (p *etcOverlayPackage) Kind() string {
	return packageKindEtcOverlay
}

func (p *etcOverlayPackage) Name() string {
	return "etc"
}

func (p *etcOverlayPackage) Version() string {
	return p.version
}

// Sources returns the dependent packages formatted as <kind>://<name>,
// sorted for deterministic fingerprinting. The etc files themselves are
// captured in each package's own fingerprint, so the etc overlay only
// needs to track which packages it aggregates.
func (p *etcOverlayPackage) Sources() []string {
	sources := make([]string, len(p.packages))
	for i, pkg := range p.packages {
		sources[i] = fmt.Sprintf("%s://%s", pkg.Kind(), pkg.Name())
	}
	sort.Strings(sources)
	return sources
}

// EtcFiles returns nil — the etc overlay itself doesn't declare further etc
// entries; it IS the etc tree.
func (p *etcOverlayPackage) EtcFiles() []PackageEtcFile {
	return nil
}

// Install creates the etc overlay tree under <base>/etc. For each installed
// package's EtcFiles, it creates a symlink at <base>/etc/<target> pointing
// to the absolute path <package-state-dir>/<source>.
//
// If two packages declare the same target path, Install returns an error
// naming both conflicting packages. Conflicts must be resolved in the overlay
// config — there is no implicit precedence.
func (p *etcOverlayPackage) Install(_ context.Context, base string) error {
	// First pass: detect duplicate target paths across all packages.
	type etcEntry struct {
		pkg     *InstalledPackage
		etcFile PackageEtcFile
	}
	seen := make(map[string]etcEntry)
	for _, pkg := range p.packages {
		for _, ef := range pkg.EtcFiles() {
			if existing, ok := seen[ef.Target]; ok {
				return fmt.Errorf(
					"etc target conflict: %q is declared by both package %q and package %q",
					ef.Target, existing.pkg.Name(), pkg.Name(),
				)
			}
			seen[ef.Target] = etcEntry{pkg: pkg, etcFile: ef}
		}
	}

	// Second pass: create the symlink tree now that we know there are no conflicts.
	etcRoot := filepath.Join(base, "etc")
	if err := os.MkdirAll(etcRoot, dirPermissions); err != nil {
		return fmt.Errorf("creating etc overlay root: %w", err)
	}

	for _, pkg := range p.packages {
		for _, ef := range pkg.EtcFiles() {
			linkPath := filepath.Join(etcRoot, ef.Target)
			linkTarget := filepath.Join(pkg.InstalledStatePath, ef.Source)

			// Create parent directories for nested targets (e.g. systemd/system/).
			if err := os.MkdirAll(filepath.Dir(linkPath), dirPermissions); err != nil {
				return fmt.Errorf("creating directory for etc symlink %q: %w", ef.Target, err)
			}

			if err := os.Symlink(linkTarget, linkPath); err != nil {
				return fmt.Errorf("creating etc symlink %q -> %q: %w", linkPath, linkTarget, err)
			}
		}
	}

	return nil
}
