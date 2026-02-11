package units

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEtcOverlayPackage_NameAndVersion(t *testing.T) {
	pkg := newEtcOverlayPackage("v1", nil)
	if pkg.Name() != "etc" {
		t.Errorf("Name() = %q, want %q", pkg.Name(), "etc")
	}
	if pkg.Version() != "v1" {
		t.Errorf("Version() = %q, want %q", pkg.Version(), "v1")
	}
}

func TestEtcOverlayPackage_EtcFiles_ReturnsNil(t *testing.T) {
	pkg := newEtcOverlayPackage("v1", nil)
	if ef := pkg.EtcFiles(); ef != nil {
		t.Errorf("EtcFiles() = %v, want nil", ef)
	}
}

func TestEtcOverlayPackage_Sources(t *testing.T) {
	depA := &InstalledPackage{
		Package:            newSystemdUnitPackage("kubelet", "1.0.0", nil, "dummy"),
		InstalledStatePath: "/aks-flex/states/kubelet-abc",
	}
	depB := &InstalledPackage{
		Package: &fakePackageWithEtcFiles{
			name:    "containerd",
			version: "1.7.0",
			etcFiles: []PackageEtcFile{
				{Source: "etc/containerd/config.toml", Target: "containerd/config.toml"},
			},
		},
		InstalledStatePath: "/aks-flex/states/containerd-def",
	}

	pkg := newEtcOverlayPackage("v1", []*InstalledPackage{depA, depB})
	sources := pkg.Sources()

	// Should include one entry per package as <kind>://<name>, sorted.
	if len(sources) != 2 {
		t.Fatalf("Sources() returned %d entries, want 2", len(sources))
	}
	if sources[0] != "source://containerd" {
		t.Errorf("Sources()[0] = %q, want %q", sources[0], "source://containerd")
	}
	if sources[1] != "systemd-unit://kubelet" {
		t.Errorf("Sources()[1] = %q, want %q", sources[1], "systemd-unit://kubelet")
	}
}

func TestEtcOverlayPackage_Install_CreatesSymlinks(t *testing.T) {
	// Set up two fake installed packages with real state dirs and files.
	stateA := filepath.Join(t.TempDir(), "kubelet-abc")
	os.MkdirAll(stateA, 0755)
	os.WriteFile(filepath.Join(stateA, "kubelet.service"), []byte("[Unit]\nDescription=Kubelet"), 0644)

	stateB := filepath.Join(t.TempDir(), "containerd-def")
	os.MkdirAll(filepath.Join(stateB, "etc", "containerd"), 0755)
	os.WriteFile(filepath.Join(stateB, "etc", "containerd", "config.toml"), []byte("key = \"value\""), 0644)

	pkgA := &InstalledPackage{
		Package:            newSystemdUnitPackage("kubelet", "1.0.0", nil, "dummy"),
		InstalledStatePath: stateA,
	}
	pkgB := &InstalledPackage{
		Package: &fakePackageWithEtcFiles{
			name:    "containerd",
			version: "1.7.0",
			etcFiles: []PackageEtcFile{
				{Source: "etc/containerd/config.toml", Target: "containerd/config.toml"},
			},
		},
		InstalledStatePath: stateB,
	}

	etcPkg := newEtcOverlayPackage("v1", []*InstalledPackage{pkgA, pkgB})

	base := filepath.Join(t.TempDir(), "etc-overlay")
	if err := etcPkg.Install(context.Background(), base); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	// Check kubelet.service symlink.
	kubeletLink := filepath.Join(base, "etc", "systemd", "system", "kubelet.service")
	target, err := os.Readlink(kubeletLink)
	if err != nil {
		t.Fatalf("Readlink(%q) error = %v", kubeletLink, err)
	}
	wantTarget := filepath.Join(stateA, "kubelet.service")
	if target != wantTarget {
		t.Errorf("kubelet symlink target = %q, want %q", target, wantTarget)
	}

	// Check containerd config symlink.
	containerdLink := filepath.Join(base, "etc", "containerd", "config.toml")
	target, err = os.Readlink(containerdLink)
	if err != nil {
		t.Fatalf("Readlink(%q) error = %v", containerdLink, err)
	}
	wantTarget = filepath.Join(stateB, "etc", "containerd", "config.toml")
	if target != wantTarget {
		t.Errorf("containerd symlink target = %q, want %q", target, wantTarget)
	}

	// Verify symlinks resolve to actual file content.
	gotContent, err := os.ReadFile(kubeletLink)
	if err != nil {
		t.Fatalf("reading through kubelet symlink: %v", err)
	}
	if string(gotContent) != "[Unit]\nDescription=Kubelet" {
		t.Errorf("kubelet content through symlink = %q", gotContent)
	}
}

func TestEtcOverlayPackage_Install_NoPackages(t *testing.T) {
	etcPkg := newEtcOverlayPackage("v1", nil)

	base := filepath.Join(t.TempDir(), "etc-overlay")
	if err := etcPkg.Install(context.Background(), base); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	// The etc/ dir should exist but be empty.
	entries, err := os.ReadDir(filepath.Join(base, "etc"))
	if err != nil {
		t.Fatalf("reading etc dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty etc dir, got %d entries", len(entries))
	}
}

func TestEtcOverlayPackage_Install_NoEtcFiles(t *testing.T) {
	// Package exists but declares no etc files.
	dep := &InstalledPackage{
		Package: &fakePackageWithEtcFiles{
			name:     "noop",
			version:  "1.0.0",
			etcFiles: nil,
		},
		InstalledStatePath: t.TempDir(),
	}

	etcPkg := newEtcOverlayPackage("v1", []*InstalledPackage{dep})

	base := filepath.Join(t.TempDir(), "etc-overlay")
	if err := etcPkg.Install(context.Background(), base); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	entries, _ := os.ReadDir(filepath.Join(base, "etc"))
	if len(entries) != 0 {
		t.Errorf("expected empty etc dir, got %d entries", len(entries))
	}
}

func TestEtcOverlayPackage_Install_DuplicateTargetConflict(t *testing.T) {
	pkgA := &InstalledPackage{
		Package: &fakePackageWithEtcFiles{
			name:    "foo",
			version: "1.0.0",
			etcFiles: []PackageEtcFile{
				{Source: "foo.conf", Target: "shared/config.toml"},
			},
		},
		InstalledStatePath: t.TempDir(),
	}
	pkgB := &InstalledPackage{
		Package: &fakePackageWithEtcFiles{
			name:    "bar",
			version: "2.0.0",
			etcFiles: []PackageEtcFile{
				{Source: "bar.conf", Target: "shared/config.toml"},
			},
		},
		InstalledStatePath: t.TempDir(),
	}

	etcPkg := newEtcOverlayPackage("v1", []*InstalledPackage{pkgA, pkgB})

	base := filepath.Join(t.TempDir(), "etc-overlay")
	err := etcPkg.Install(context.Background(), base)
	if err == nil {
		t.Fatal("Install() should have returned an error for duplicate etc targets")
	}

	// Verify the error message names both packages and the conflicting target.
	wantSubstrings := []string{"shared/config.toml", "foo", "bar", "conflict"}
	for _, sub := range wantSubstrings {
		if !strings.Contains(err.Error(), sub) {
			t.Errorf("error %q should contain %q", err.Error(), sub)
		}
	}

	// Verify no symlinks were created (detection happens before any I/O).
	if _, statErr := os.Stat(filepath.Join(base, "etc")); !os.IsNotExist(statErr) {
		t.Error("expected etc dir to not exist since conflict was detected before creating anything")
	}
}

// fakePackageWithEtcFiles is a test helper implementing Package with
// configurable EtcFiles.
type fakePackageWithEtcFiles struct {
	name     string
	version  string
	etcFiles []PackageEtcFile
}

func (f *fakePackageWithEtcFiles) Kind() string                          { return packageKindSource }
func (f *fakePackageWithEtcFiles) Name() string                          { return f.name }
func (f *fakePackageWithEtcFiles) Version() string                       { return f.version }
func (f *fakePackageWithEtcFiles) Sources() []string                     { return nil }
func (f *fakePackageWithEtcFiles) EtcFiles() []PackageEtcFile            { return f.etcFiles }
func (f *fakePackageWithEtcFiles) Install(context.Context, string) error { return nil }
