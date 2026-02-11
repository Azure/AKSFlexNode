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
	if info, err := os.Stat(stateDir); err == nil && info.IsDir() {
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

	if err := o.prepareOverlayPackages(ctx); err != nil {
		return fmt.Errorf("preparing overlay packages: %w", err)
	}

	return nil
}

// prepareOverlayPackages resolves each package defined in overlay config and installs
// it into the store via StoreManager.installPackage.
//
// TODO: process packages concurrently (e.g. via an errgroup) once Install
// implementations are confirmed safe for parallel execution.
func (o *Overlay) prepareOverlayPackages(ctx context.Context) error {
	for name, def := range o.config.PackageByNames {
		pkg, err := newOverlayPackage(name, def)
		if err != nil {
			return fmt.Errorf("creating package %q: %w", name, err)
		}

		if _, err := o.store.installPackage(ctx, pkg); err != nil {
			return err
		}
	}

	return nil
}

func (o *Overlay) Apply(ctx context.Context) error {
	return nil
}
