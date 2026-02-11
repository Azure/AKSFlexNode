package units

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// DefaultStoreRoot is the default root directory for the AKS Flex store.
	DefaultStoreRoot = "/aks-flex"

	// configsDir is the subdirectory under the store root for versioned config files.
	configsDir = "configs"

	// statesDir is the subdirectory under the store root for package state directories.
	statesDir = "states"

	// dirPermissions is the default permission mode for created directories.
	dirPermissions = 0755
)

// dirExists reports whether path exists and is a directory.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// StoreManager manages the on-disk store layout for AKS Flex node packages and configs.
type StoreManager struct {
	// root is the absolute path to the store root directory (e.g. /aks-flex).
	root string
}

// NewStoreManager creates a new StoreManager with the given root directory.
// If root is empty, DefaultStoreRoot is used.
func NewStoreManager(root string) *StoreManager {
	if root == "" {
		root = DefaultStoreRoot
	}
	return &StoreManager{root: root}
}

// setupDiskLayout creates the expected directory structure under the store root.
//
// The layout follows the structure described in docs/notes.md:
//
//	<root>/          <- store root (e.g. /aks-flex)
//	  |- configs/    <- versioned config files
//	  |- states/     <- package state directories
func (mgr *StoreManager) setupDiskLayout() error {
	dirs := []string{
		mgr.root,
		filepath.Join(mgr.root, configsDir),
		filepath.Join(mgr.root, statesDir),
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, dirPermissions); err != nil {
			return fmt.Errorf("creating directory %s: %w", dir, err)
		}
	}

	return nil
}

// Prepare sets up the store disk layout and persists the overlay config
// as a versioned JSON file under <root>/configs/<version>.json.
func (mgr *StoreManager) Prepare(overlayConfig OverlayConfig) error {
	if err := mgr.setupDiskLayout(); err != nil {
		return fmt.Errorf("setting up disk layout: %w", err)
	}

	if overlayConfig.Version == "" {
		return fmt.Errorf("overlay config version is required")
	}

	configPath := filepath.Join(mgr.root, configsDir, overlayConfig.Version+".json")
	data, err := json.MarshalIndent(overlayConfig, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling overlay config: %w", err)
	}

	if err := os.WriteFile(configPath, data, 0644); err != nil {
		return fmt.Errorf("writing config file %s: %w", configPath, err)
	}

	return nil
}

// installPackage installs a package into the store's states directory.
// It computes the expected state directory name from the package fingerprint,
// skips if it already exists (cached), otherwise installs into a temporary
// directory and atomically renames it to the final location.
//
// It returns the absolute path to the package's state directory.
func (mgr *StoreManager) installPackage(ctx context.Context, pkg Package) (string, error) {
	statesRoot := filepath.Join(mgr.root, statesDir)
	stateDir := filepath.Join(statesRoot, packageStateDirName(pkg))

	// Skip if this exact package version is already installed.
	if dirExists(stateDir) {
		return stateDir, nil
	}

	// Install into a temporary directory next to the final location so
	// os.Rename is an atomic same-filesystem move.
	tmpDir, err := os.MkdirTemp(statesRoot, pkg.Name()+"-tmp-*")
	if err != nil {
		return "", fmt.Errorf("creating temp dir for package %q: %w", pkg.Name(), err)
	}

	if err := pkg.Install(ctx, tmpDir); err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("installing package %q: %w", pkg.Name(), err)
	}

	if err := os.Rename(tmpDir, stateDir); err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("moving package %q to state dir: %w", pkg.Name(), err)
	}

	return stateDir, nil
}

func calculatePackageFingerprint(pkg Package) string {
	hasher := sha256.New()

	fmt.Fprintf(hasher, "%s|%s", pkg.Name(), pkg.Version())

	sources := pkg.Sources()
	sort.Strings(sources)
	fmt.Fprintf(hasher, "|%s", strings.Join(sources, ","))

	var etcFiles []string
	for _, etcFile := range pkg.EtcFiles() {
		etcFiles = append(etcFiles, fmt.Sprintf("%s|%s", etcFile.Source, etcFile.Target))
	}
	sort.Strings(etcFiles)
	fmt.Fprintf(hasher, "|%s", strings.Join(etcFiles, ","))

	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(hasher.Sum(nil)))
}

// packageStateDirName returns the directory name for a package in the states dir.
// Format: <name>-<fingerprint>
func packageStateDirName(pkg Package) string {
	fingerprint := calculatePackageFingerprint(pkg)
	return fmt.Sprintf("%s-%s", pkg.Name(), fingerprint)
}

type Overlay struct {
	config OverlayConfig

	store *StoreManager
	// TODO: etc manager / systemd manager
}

func (o *Overlay) Prepare(ctx context.Context) error {
	if err := o.store.Prepare(o.config); err != nil {
		return fmt.Errorf("setting up disk layout: %w", err)
	}

	installedPkgs, err := o.prepareOverlayPackages(ctx)
	if err != nil {
		return fmt.Errorf("preparing overlay packages: %w", err)
	}

	unitPkgs, err := o.prepareSystemdUnits(ctx, installedPkgs)
	if err != nil {
		return fmt.Errorf("preparing systemd units: %w", err)
	}

	// Collect all installed packages for the etc overlay.
	allPkgs := make([]*installedPackage, 0, len(installedPkgs)+len(unitPkgs))
	for _, pkg := range installedPkgs {
		allPkgs = append(allPkgs, pkg)
	}
	allPkgs = append(allPkgs, unitPkgs...)

	if err := o.prepareEtcOverlay(ctx, allPkgs); err != nil {
		return fmt.Errorf("preparing etc overlay: %w", err)
	}

	return nil
}

type installedPackage struct {
	Package
	InstalledStatePath string
}

func (p *installedPackage) BinPaths() []string {
	var binPaths []string
	binDir := filepath.Join(p.InstalledStatePath, "bin")
	if dirExists(binDir) {
		binPaths = append(binPaths, binDir)
	}
	return binPaths
}

// prepareOverlayPackages resolves each package defined in overlay config and installs
// it into the store via StoreManager.installPackage. It returns a map of package
// name to installedPackage so callers (e.g. systemd unit preparation) can look up
// dependencies and access installed state paths.
//
// TODO: process packages concurrently (e.g. via an errgroup) once Install
// implementations are confirmed safe for parallel execution.
func (o *Overlay) prepareOverlayPackages(ctx context.Context) (map[string]*installedPackage, error) {
	installed := make(map[string]*installedPackage, len(o.config.PackageByNames))

	for name, def := range o.config.PackageByNames {
		pkg, err := newOverlayPackage(name, def)
		if err != nil {
			return nil, fmt.Errorf("creating package %q: %w", name, err)
		}

		stateDir, err := o.store.installPackage(ctx, pkg)
		if err != nil {
			return nil, err
		}

		installed[name] = &installedPackage{
			Package:            pkg,
			InstalledStatePath: stateDir,
		}
	}

	return installed, nil
}

// prepareSystemdUnits resolves each systemd unit defined in overlay config,
// resolves its template content and package dependencies, creates a
// systemdUnitPackage and installs it into the store. Returns the installed
// systemd unit packages so they can be included in the etc overlay.
func (o *Overlay) prepareSystemdUnits(ctx context.Context, installedPkgs map[string]*installedPackage) ([]*installedPackage, error) {
	var unitPkgs []*installedPackage

	for name, def := range o.config.SystemdUnitsByName {
		template, err := resolveSystemdTemplate(def)
		if err != nil {
			return nil, fmt.Errorf("resolving template for systemd unit %q: %w", name, err)
		}

		packages, err := resolvePackageRefs(name, def.Packages, installedPkgs)
		if err != nil {
			return nil, err
		}

		pkg := newSystemdUnitPackage(name, def.Version, packages, template)

		stateDir, err := o.store.installPackage(ctx, pkg)
		if err != nil {
			return nil, err
		}

		unitPkgs = append(unitPkgs, &installedPackage{
			Package:            pkg,
			InstalledStatePath: stateDir,
		})
	}

	return unitPkgs, nil
}

// prepareEtcOverlay builds and installs the etc overlay package, which
// collects all EtcFiles from the given installed packages and creates a
// unified symlink tree under <state>/etc/.
func (o *Overlay) prepareEtcOverlay(ctx context.Context, packages []*installedPackage) error {
	etcPkg := newEtcOverlayPackage(o.config.Version, packages)

	if _, err := o.store.installPackage(ctx, etcPkg); err != nil {
		return err
	}

	return nil
}

// resolveSystemdTemplate reads the template content for a systemd unit definition.
// Exactly one of TemplateFile or TemplateInline must be set.
func resolveSystemdTemplate(def OverlaySystemdUnitDef) (string, error) {
	hasFile := def.TemplateFile != ""
	hasInline := def.TemplateInline != ""

	if hasFile && hasInline {
		return "", fmt.Errorf("templateFile and templateInline are mutually exclusive")
	}
	if !hasFile && !hasInline {
		return "", fmt.Errorf("templateFile or templateInline is required")
	}

	if hasInline {
		return def.TemplateInline, nil
	}

	data, err := os.ReadFile(def.TemplateFile)
	if err != nil {
		return "", fmt.Errorf("reading template file %q: %w", def.TemplateFile, err)
	}
	return string(data), nil
}

// resolvePackageRefs looks up package names from the installed packages map and
// returns the corresponding Package slice. Returns an error if any referenced
// package was not found.
func resolvePackageRefs(unitName string, packageNames []string, installed map[string]*installedPackage) ([]*installedPackage, error) {
	packages := make([]*installedPackage, 0, len(packageNames))
	for _, pkgName := range packageNames {
		pkg, ok := installed[pkgName]
		if !ok {
			return nil, fmt.Errorf("systemd unit %q references unknown package %q", unitName, pkgName)
		}
		packages = append(packages, pkg)
	}
	return packages, nil
}

func (o *Overlay) Apply(ctx context.Context) error {
	return nil
}
