package units

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
)

// systemdUnitPackage implements Package for a systemd unit definition.
// Install produces a unit file at <name>.service directly under the state
// directory. The EtcFiles mapping handles symlinking it into
// /etc/systemd/system/.
type systemdUnitPackage struct {
	name     string
	version  string
	packages []*InstalledPackage
	template string
}

func newSystemdUnitPackage(name, version string, packages []*InstalledPackage, template string) *systemdUnitPackage {
	return &systemdUnitPackage{
		name:     name,
		version:  version,
		packages: packages,
		template: template,
	}
}

var _ Package = (*systemdUnitPackage)(nil)

func (s *systemdUnitPackage) Kind() string {
	return packageKindSystemdUnit
}

func (s *systemdUnitPackage) Name() string {
	return s.name
}

func (s *systemdUnitPackage) Version() string {
	return s.version
}

// Sources returns the dependent packages formatted as <kind>://<name>,
// sorted for deterministic fingerprinting.
func (s *systemdUnitPackage) Sources() []string {
	sources := make([]string, len(s.packages))
	for i, pkg := range s.packages {
		sources[i] = packageSource(pkg)
	}
	sort.Strings(sources)
	return sources
}

// EtcFiles declares the unit file so the etc manager can symlink it into /etc.
func (s *systemdUnitPackage) EtcFiles() []PackageEtcFile {
	serviceFile := s.name + ".service"
	return []PackageEtcFile{
		{
			Source: serviceFile,
			Target: filepath.Join("systemd", "system", serviceFile),
		},
	}
}

// unitTemplateContext provides helper functions available inside systemd unit
// templates. It is passed as the data argument to text/template.Execute.
type unitTemplateContext struct {
	packages []*InstalledPackage
}

// defaultSystemPaths are the standard system PATH directories. These are NOT
// included by default — use GetPathEnvWithSystemDefaults explicitly if needed.
// Including these breaks the hermetic package model; prefer declaring all
// dependencies through packages instead.
var defaultSystemPaths = []string{
	"/usr/local/sbin",
	"/usr/local/bin",
	"/usr/sbin",
	"/usr/bin",
	"/sbin",
	"/bin",
}

// GetPathEnv returns all bin paths from the referenced packages, merged,
// sorted and joined with ":". Only declared package bin paths are included;
// no system defaults are appended, ensuring a hermetic PATH.
// Usage: {{ .GetPathEnv }}
func (c *unitTemplateContext) GetPathEnv() string {
	var all []string
	for _, pkg := range c.packages {
		all = append(all, pkg.BinPaths()...)
	}
	sort.Strings(all)
	return strings.Join(all, ":")
}

// GetPathEnvWithSystemDefaults returns GetPathEnv with standard system PATH
// directories (/usr/local/sbin, /usr/local/bin, etc.) appended at the end.
// This is an escape hatch for units that need access to system binaries not
// managed through packages. Prefer GetPathEnv and explicit package
// dependencies where possible.
// Usage: {{ .GetPathEnvWithSystemDefaults }}
func (c *unitTemplateContext) GetPathEnvWithSystemDefaults() string {
	base := c.GetPathEnv()
	suffix := strings.Join(defaultSystemPaths, ":")
	if base == "" {
		return suffix
	}
	return base + ":" + suffix
}

// GetPackagePath returns the absolute path into a referenced package's state
// directory, optionally joined with sub-path components. It returns an error
// (which text/template surfaces) if the package name is not found.
func (c *unitTemplateContext) GetPackagePath(packageName string, subPaths ...string) (string, error) {
	for _, pkg := range c.packages {
		if pkg.Name() == packageName {
			parts := append([]string{pkg.InstalledStatePath}, subPaths...)
			return filepath.Join(parts...), nil
		}
	}
	return "", fmt.Errorf("package %q not found in referenced packages", packageName)
}

// renderTemplate executes the unit template with the helper functions
// provided by unitTemplateContext.
func (s *systemdUnitPackage) renderTemplate() (string, error) {
	tmpl, err := template.New(s.name).Parse(s.template)
	if err != nil {
		return "", fmt.Errorf("parsing template for systemd unit %q: %w", s.name, err)
	}

	ctx := &unitTemplateContext{packages: s.packages}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, ctx); err != nil {
		return "", fmt.Errorf("executing template for systemd unit %q: %w", s.name, err)
	}

	return buf.String(), nil
}

// Install renders the template and writes the systemd unit file to <base>/<name>.service.
func (s *systemdUnitPackage) Install(_ context.Context, base string) error {
	if err := os.MkdirAll(base, dirPermissions); err != nil {
		return fmt.Errorf("creating base directory for systemd unit %q: %w", s.name, err)
	}

	content, err := s.renderTemplate()
	if err != nil {
		return err
	}

	unitPath := filepath.Join(base, s.name+".service")

	if err := os.WriteFile(unitPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("writing systemd unit file %q: %w", unitPath, err)
	}

	return nil
}
