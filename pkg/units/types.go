package units

import "context"

// PackageEtcFile defines a file that needs to be placed in /etc as part of the package installation.
//
// Example:
//
//	PackageEtcFile{
//	    Source: "etc/containerd/config.toml",
//	    Target: "containerd/config.toml",
//	}
//
// the installation of the package will result in the file being placed in /etc/containerd/config.toml using symlink.
type PackageEtcFile struct {
	// Source is the relative path of the file in the package artifacts.
	Source string `json:"source"`
	// Target is the relative path of the file in the /etc directory.
	Target string `json:"target"`
}

// Package defines a package unit to be used in the node host.
type Package interface {
	// Name returns the name of the package.
	// Ex: "containerd", "runc", "kubelet", etc.
	Name() string
	// Version returns the version of the package.
	// Ex: "1.6.21", "1.1.4", etc.
	Version() string
	// Sources returns the list of sources (e.g. URLs) required for this package.
	// This is used for identifying the dependencies / sources and will be used
	// for calculating consistent fingerprint for the package.
	Sources() []string

	// Install installs the package under the provided base path.
	Install(ctx context.Context, base string) error

	// EtcFiles returns the list of files that need to be placed in /etc as part of
	// the package installation.
	EtcFiles() []PackageEtcFile
}

type SystemdUnit struct { // -> this is also a Package
	// Name is the name of the systemd unit, e.g. "kubelet" -> "kubelet.service"
	Name string
	// Packages defines the list of packages that this systemd unit depends on.
	// It will be resolved with the absolute package binary paths and exposed
	// as PATH environment variable in the systemd unit for use in ExecStart and other commands.
	Packages []Package
	// UnitTemplate is the template string for rendering the system unit file.
	UnitTemplate string
}

// 1. resolve packages (download or reuse from cache)
// 2. produce overlay
// 3. symlink overlay
// 4. update systemd units
