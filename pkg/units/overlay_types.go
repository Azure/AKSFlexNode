package units

// OverlayPackageSource describes where to fetch a package from.
type OverlayPackageSource struct {
	Type string `json:"type"` // e.g. "url", "url+tar", "url+zip", "file", etc.
	URI  string `json:"uri"`  // e.g. the actual URL or file path
	// TODO: add checksum for validation
}

// OverlaySystemdUnitDef defines a systemd unit in the overlay config.
type OverlaySystemdUnitDef struct {
	Version        string   `json:"version"`                  // version of the systemd unit definition
	Packages       []string `json:"packages"`                 // reference PackagesByName
	TemplateFile   string   `json:"templateFile,omitempty"`   // path to the systemd unit template file
	TemplateInline string   `json:"templateInline,omitempty"` // inline systemd unit template content (alternative to TemplateFile)
}

type OverlayPackageDef struct {
	Version  string               `json:"version"`
	Source   OverlayPackageSource `json:"source"`
	ETCFiles []PackageEtcFile     `json:"etcFiles,omitempty"`
}

type OverlayConfig struct { // JSON config
	Version string `json:"version"` // Unique version across all overlay configs

	PackagesByName     map[string]OverlayPackageDef     `json:"packagesByName,omitempty"`
	SystemdUnitsByName map[string]OverlaySystemdUnitDef `json:"systemdUnitsByName,omitempty"`
}
