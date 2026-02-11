package units

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEtcManager_SymlinkToStatic_CreatesSymlink(t *testing.T) {
	rootDir := t.TempDir()
	mgr := newEtcManager(rootDir)

	source := filepath.Join(t.TempDir(), "etc-overlay", "etc")
	os.MkdirAll(source, 0755)

	err := mgr.SymlinkToStatic(source)
	if err != nil {
		t.Fatalf("SymlinkToStatic() error = %v", err)
	}

	wantPath := filepath.Join(rootDir, "etc", "static")

	dest, err := os.Readlink(wantPath)
	if err != nil {
		t.Fatalf("Readlink() error = %v", err)
	}
	if dest != source {
		t.Errorf("symlink target = %q, want %q", dest, source)
	}
}

func TestEtcManager_SymlinkToStatic_ReplacesExisting(t *testing.T) {
	rootDir := t.TempDir()
	mgr := newEtcManager(rootDir)

	source1 := filepath.Join(t.TempDir(), "overlay-v1", "etc")
	os.MkdirAll(source1, 0755)
	source2 := filepath.Join(t.TempDir(), "overlay-v2", "etc")
	os.MkdirAll(source2, 0755)

	if err := mgr.SymlinkToStatic(source1); err != nil {
		t.Fatalf("first SymlinkToStatic() error = %v", err)
	}

	if err := mgr.SymlinkToStatic(source2); err != nil {
		t.Fatalf("second SymlinkToStatic() error = %v", err)
	}

	dest, err := os.Readlink(mgr.staticPath())
	if err != nil {
		t.Fatalf("Readlink() error = %v", err)
	}
	if dest != source2 {
		t.Errorf("symlink target = %q, want %q (should have been replaced)", dest, source2)
	}
}

func TestEtcManager_SymlinkToStatic_CreatesEtcDir(t *testing.T) {
	rootDir := filepath.Join(t.TempDir(), "nonexistent")
	mgr := newEtcManager(rootDir)

	source := filepath.Join(t.TempDir(), "overlay", "etc")
	os.MkdirAll(source, 0755)

	if err := mgr.SymlinkToStatic(source); err != nil {
		t.Fatalf("SymlinkToStatic() error = %v", err)
	}

	info, err := os.Stat(filepath.Join(rootDir, "etc"))
	if err != nil {
		t.Fatalf("etc dir should exist: %v", err)
	}
	if !info.IsDir() {
		t.Error("etc should be a directory")
	}
}

func TestEtcManager_PromoteStaticToEtc_CreatesSymlinks(t *testing.T) {
	rootDir := t.TempDir()
	mgr := newEtcManager(rootDir)

	// Set up a static dir with files.
	overlayDir := filepath.Join(t.TempDir(), "etc")
	os.MkdirAll(filepath.Join(overlayDir, "containerd"), 0755)
	os.WriteFile(filepath.Join(overlayDir, "containerd", "config.toml"), []byte("cfg"), 0644)
	os.WriteFile(filepath.Join(overlayDir, "hosts"), []byte("hosts-data"), 0644)

	if err := mgr.SymlinkToStatic(overlayDir); err != nil {
		t.Fatalf("SymlinkToStatic() error = %v", err)
	}

	if err := mgr.PromoteStaticToEtc(); err != nil {
		t.Fatalf("PromoteStaticToEtc() error = %v", err)
	}

	// Check containerd/config.toml symlink.
	linkPath := filepath.Join(rootDir, "etc", "containerd", "config.toml")
	dest, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatalf("Readlink(%s) error = %v", linkPath, err)
	}
	wantTarget := filepath.Join(rootDir, "etc", "static", "containerd", "config.toml")
	if dest != wantTarget {
		t.Errorf("symlink target = %q, want %q", dest, wantTarget)
	}

	// Verify the chain resolves to content.
	content, err := os.ReadFile(linkPath)
	if err != nil {
		t.Fatalf("reading through symlink chain: %v", err)
	}
	if string(content) != "cfg" {
		t.Errorf("content = %q, want %q", content, "cfg")
	}

	// Check hosts symlink.
	hostsContent, err := os.ReadFile(filepath.Join(rootDir, "etc", "hosts"))
	if err != nil {
		t.Fatalf("reading hosts: %v", err)
	}
	if string(hostsContent) != "hosts-data" {
		t.Errorf("hosts content = %q, want %q", hostsContent, "hosts-data")
	}
}

func TestEtcManager_PromoteStaticToEtc_CreatesParentDirs(t *testing.T) {
	rootDir := t.TempDir()
	mgr := newEtcManager(rootDir)

	overlayDir := filepath.Join(t.TempDir(), "etc")
	os.MkdirAll(filepath.Join(overlayDir, "systemd", "system"), 0755)
	os.WriteFile(filepath.Join(overlayDir, "systemd", "system", "foo.service"), []byte("unit"), 0644)

	if err := mgr.SymlinkToStatic(overlayDir); err != nil {
		t.Fatalf("SymlinkToStatic() error = %v", err)
	}

	if err := mgr.PromoteStaticToEtc(); err != nil {
		t.Fatalf("PromoteStaticToEtc() error = %v", err)
	}

	linkPath := filepath.Join(rootDir, "etc", "systemd", "system", "foo.service")
	if _, err := os.Readlink(linkPath); err != nil {
		t.Fatalf("expected symlink at %s: %v", linkPath, err)
	}
}

func TestEtcManager_PromoteStaticToEtc_Idempotent(t *testing.T) {
	rootDir := t.TempDir()
	mgr := newEtcManager(rootDir)

	overlayDir := filepath.Join(t.TempDir(), "etc")
	os.MkdirAll(overlayDir, 0755)
	os.WriteFile(filepath.Join(overlayDir, "hosts"), []byte("hosts"), 0644)

	if err := mgr.SymlinkToStatic(overlayDir); err != nil {
		t.Fatalf("SymlinkToStatic() error = %v", err)
	}

	// Call twice — second call should be a no-op.
	if err := mgr.PromoteStaticToEtc(); err != nil {
		t.Fatalf("first PromoteStaticToEtc() error = %v", err)
	}
	if err := mgr.PromoteStaticToEtc(); err != nil {
		t.Fatalf("second PromoteStaticToEtc() error = %v", err)
	}

	dest, err := os.Readlink(filepath.Join(rootDir, "etc", "hosts"))
	if err != nil {
		t.Fatalf("Readlink() error = %v", err)
	}
	wantTarget := filepath.Join(rootDir, "etc", "static", "hosts")
	if dest != wantTarget {
		t.Errorf("symlink target = %q, want %q", dest, wantTarget)
	}
}

func TestEtcManager_PromoteStaticToEtc_ReplacesStaleSymlink(t *testing.T) {
	rootDir := t.TempDir()
	mgr := newEtcManager(rootDir)

	overlayDir := filepath.Join(t.TempDir(), "etc")
	os.MkdirAll(overlayDir, 0755)
	os.WriteFile(filepath.Join(overlayDir, "resolv.conf"), []byte("nameserver 8.8.8.8"), 0644)

	if err := mgr.SymlinkToStatic(overlayDir); err != nil {
		t.Fatalf("SymlinkToStatic() error = %v", err)
	}

	// Create a stale symlink pointing somewhere else.
	etcDir := filepath.Join(rootDir, "etc")
	os.MkdirAll(etcDir, 0755)
	os.Symlink("/some/old/path", filepath.Join(etcDir, "resolv.conf"))

	// PromoteStaticToEtc should replace it.
	if err := mgr.PromoteStaticToEtc(); err != nil {
		t.Fatalf("PromoteStaticToEtc() error = %v", err)
	}

	dest, err := os.Readlink(filepath.Join(rootDir, "etc", "resolv.conf"))
	if err != nil {
		t.Fatalf("Readlink() error = %v", err)
	}
	wantTarget := filepath.Join(rootDir, "etc", "static", "resolv.conf")
	if dest != wantTarget {
		t.Errorf("symlink target = %q, want %q", dest, wantTarget)
	}
}

func TestEtcManager_PromoteStaticToEtc_RefusesNonSymlink(t *testing.T) {
	rootDir := t.TempDir()
	mgr := newEtcManager(rootDir)

	overlayDir := filepath.Join(t.TempDir(), "etc")
	os.MkdirAll(overlayDir, 0755)
	os.WriteFile(filepath.Join(overlayDir, "important.conf"), []byte("overlay-version"), 0644)

	if err := mgr.SymlinkToStatic(overlayDir); err != nil {
		t.Fatalf("SymlinkToStatic() error = %v", err)
	}

	// Create a real file at the target location.
	etcDir := filepath.Join(rootDir, "etc")
	os.WriteFile(filepath.Join(etcDir, "important.conf"), []byte("do not touch"), 0644)

	err := mgr.PromoteStaticToEtc()
	if err == nil {
		t.Fatal("PromoteStaticToEtc() should report error for non-symlink conflict")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Errorf("error = %q, want it to mention refusing to overwrite", err)
	}
}

func TestEtcManager_PromoteStaticToEtc_RemovesStaleEntries(t *testing.T) {
	rootDir := t.TempDir()
	mgr := newEtcManager(rootDir)

	// Gen 1: two files.
	overlay1 := filepath.Join(t.TempDir(), "etc")
	os.MkdirAll(filepath.Join(overlay1, "systemd", "system"), 0755)
	os.WriteFile(filepath.Join(overlay1, "hosts"), []byte("hosts"), 0644)
	os.WriteFile(filepath.Join(overlay1, "systemd", "system", "foo.service"), []byte("foo"), 0644)

	if err := mgr.SymlinkToStatic(overlay1); err != nil {
		t.Fatalf("gen1 SymlinkToStatic() error = %v", err)
	}
	if err := mgr.PromoteStaticToEtc(); err != nil {
		t.Fatalf("gen1 PromoteStaticToEtc() error = %v", err)
	}

	// Verify both exist.
	for _, p := range []string{"hosts", "systemd/system/foo.service"} {
		if _, err := os.Lstat(filepath.Join(rootDir, "etc", p)); err != nil {
			t.Fatalf("gen1: expected %s to exist: %v", p, err)
		}
	}

	// Gen 2: only hosts (foo.service removed).
	overlay2 := filepath.Join(t.TempDir(), "etc")
	os.MkdirAll(overlay2, 0755)
	os.WriteFile(filepath.Join(overlay2, "hosts"), []byte("hosts-v2"), 0644)

	if err := mgr.SymlinkToStatic(overlay2); err != nil {
		t.Fatalf("gen2 SymlinkToStatic() error = %v", err)
	}
	if err := mgr.PromoteStaticToEtc(); err != nil {
		t.Fatalf("gen2 PromoteStaticToEtc() error = %v", err)
	}

	// hosts should still exist.
	if _, err := os.Lstat(filepath.Join(rootDir, "etc", "hosts")); err != nil {
		t.Errorf("expected hosts to still exist: %v", err)
	}

	// foo.service should be gone.
	fooPath := filepath.Join(rootDir, "etc", "systemd", "system", "foo.service")
	if _, err := os.Lstat(fooPath); !os.IsNotExist(err) {
		t.Errorf("expected %s to be removed, got err = %v", fooPath, err)
	}
}

func TestEtcManager_PromoteStaticToEtc_CleansEmptyParentDirs(t *testing.T) {
	rootDir := t.TempDir()
	mgr := newEtcManager(rootDir)

	// Gen 1: deep nested file.
	overlay1 := filepath.Join(t.TempDir(), "etc")
	os.MkdirAll(filepath.Join(overlay1, "deep", "nested"), 0755)
	os.WriteFile(filepath.Join(overlay1, "deep", "nested", "file.conf"), []byte("data"), 0644)

	if err := mgr.SymlinkToStatic(overlay1); err != nil {
		t.Fatalf("gen1 SymlinkToStatic() error = %v", err)
	}
	if err := mgr.PromoteStaticToEtc(); err != nil {
		t.Fatalf("gen1 PromoteStaticToEtc() error = %v", err)
	}

	// Gen 2: empty overlay (everything removed).
	overlay2 := filepath.Join(t.TempDir(), "etc")
	os.MkdirAll(overlay2, 0755)

	if err := mgr.SymlinkToStatic(overlay2); err != nil {
		t.Fatalf("gen2 SymlinkToStatic() error = %v", err)
	}
	if err := mgr.PromoteStaticToEtc(); err != nil {
		t.Fatalf("gen2 PromoteStaticToEtc() error = %v", err)
	}

	// The deep/nested/ dirs should be cleaned up.
	deepDir := filepath.Join(rootDir, "etc", "deep")
	if _, err := os.Stat(deepDir); !os.IsNotExist(err) {
		t.Errorf("expected %s to be removed (empty parent cleanup), got err = %v", deepDir, err)
	}
}

func TestEtcManager_PromoteStaticToEtc_SkipsNonManagedSymlinks(t *testing.T) {
	rootDir := t.TempDir()
	mgr := newEtcManager(rootDir)

	// Gen 1: has "manual.conf".
	overlay1 := filepath.Join(t.TempDir(), "etc")
	os.MkdirAll(overlay1, 0755)
	os.WriteFile(filepath.Join(overlay1, "manual.conf"), []byte("managed"), 0644)

	if err := mgr.SymlinkToStatic(overlay1); err != nil {
		t.Fatalf("gen1 SymlinkToStatic() error = %v", err)
	}
	if err := mgr.PromoteStaticToEtc(); err != nil {
		t.Fatalf("gen1 PromoteStaticToEtc() error = %v", err)
	}

	// Now manually replace the /etc/manual.conf symlink to point elsewhere
	// (simulating user intervention), and switch to gen2 without manual.conf.
	os.Remove(filepath.Join(rootDir, "etc", "manual.conf"))
	os.Symlink("/some/other/path", filepath.Join(rootDir, "etc", "manual.conf"))

	overlay2 := filepath.Join(t.TempDir(), "etc")
	os.MkdirAll(overlay2, 0755)

	if err := mgr.SymlinkToStatic(overlay2); err != nil {
		t.Fatalf("gen2 SymlinkToStatic() error = %v", err)
	}
	if err := mgr.PromoteStaticToEtc(); err != nil {
		t.Fatalf("gen2 PromoteStaticToEtc() error = %v", err)
	}

	// The symlink should still exist because it no longer points into static.
	if _, err := os.Lstat(filepath.Join(rootDir, "etc", "manual.conf")); err != nil {
		t.Errorf("expected non-managed symlink to be left alone: %v", err)
	}
}

func TestEtcManager_PromoteStaticToEtc_EmptyStaticDir(t *testing.T) {
	rootDir := t.TempDir()
	mgr := newEtcManager(rootDir)

	overlayDir := filepath.Join(t.TempDir(), "etc")
	os.MkdirAll(overlayDir, 0755)

	if err := mgr.SymlinkToStatic(overlayDir); err != nil {
		t.Fatalf("SymlinkToStatic() error = %v", err)
	}
	if err := mgr.PromoteStaticToEtc(); err != nil {
		t.Fatalf("PromoteStaticToEtc() error = %v", err)
	}

	// /etc should exist but contain only the static symlink (no promoted entries).
	entries, err := os.ReadDir(filepath.Join(rootDir, "etc"))
	if err != nil {
		t.Fatalf("reading etc dir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "static" {
			t.Errorf("unexpected entry in /etc: %s", e.Name())
		}
	}
}

func TestEtcManager_EndToEnd_TwoGenerations(t *testing.T) {
	rootDir := t.TempDir()
	mgr := newEtcManager(rootDir)

	// --- Generation 1: containerd config + systemd unit ---
	overlay1 := filepath.Join(t.TempDir(), "etc")
	os.MkdirAll(filepath.Join(overlay1, "containerd"), 0755)
	os.MkdirAll(filepath.Join(overlay1, "systemd", "system"), 0755)
	os.WriteFile(filepath.Join(overlay1, "containerd", "config.toml"), []byte("gen1-cfg"), 0644)
	os.WriteFile(filepath.Join(overlay1, "systemd", "system", "containerd.service"), []byte("gen1-unit"), 0644)

	if err := mgr.SymlinkToStatic(overlay1); err != nil {
		t.Fatalf("gen1 SymlinkToStatic() error = %v", err)
	}
	if err := mgr.PromoteStaticToEtc(); err != nil {
		t.Fatalf("gen1 PromoteStaticToEtc() error = %v", err)
	}

	// Verify gen1 content resolves.
	content, err := os.ReadFile(filepath.Join(rootDir, "etc", "containerd", "config.toml"))
	if err != nil {
		t.Fatalf("reading gen1 containerd config: %v", err)
	}
	if string(content) != "gen1-cfg" {
		t.Errorf("gen1 containerd config = %q, want %q", content, "gen1-cfg")
	}

	// --- Generation 2: update config, remove systemd unit, add new file ---
	overlay2 := filepath.Join(t.TempDir(), "etc")
	os.MkdirAll(filepath.Join(overlay2, "containerd"), 0755)
	os.WriteFile(filepath.Join(overlay2, "containerd", "config.toml"), []byte("gen2-cfg"), 0644)
	os.WriteFile(filepath.Join(overlay2, "resolv.conf"), []byte("nameserver 1.1.1.1"), 0644)

	if err := mgr.SymlinkToStatic(overlay2); err != nil {
		t.Fatalf("gen2 SymlinkToStatic() error = %v", err)
	}
	if err := mgr.PromoteStaticToEtc(); err != nil {
		t.Fatalf("gen2 PromoteStaticToEtc() error = %v", err)
	}

	// Verify gen2 containerd config now shows updated content.
	content, err = os.ReadFile(filepath.Join(rootDir, "etc", "containerd", "config.toml"))
	if err != nil {
		t.Fatalf("reading gen2 containerd config: %v", err)
	}
	if string(content) != "gen2-cfg" {
		t.Errorf("gen2 containerd config = %q, want %q", content, "gen2-cfg")
	}

	// Verify new file exists.
	content, err = os.ReadFile(filepath.Join(rootDir, "etc", "resolv.conf"))
	if err != nil {
		t.Fatalf("reading gen2 resolv.conf: %v", err)
	}
	if string(content) != "nameserver 1.1.1.1" {
		t.Errorf("gen2 resolv.conf = %q, want %q", content, "nameserver 1.1.1.1")
	}

	// Verify stale systemd unit was removed.
	unitPath := filepath.Join(rootDir, "etc", "systemd", "system", "containerd.service")
	if _, err := os.Lstat(unitPath); !os.IsNotExist(err) {
		t.Errorf("expected stale unit %s to be removed, got err = %v", unitPath, err)
	}

	// Verify empty systemd/system/ and systemd/ dirs were cleaned up.
	systemdDir := filepath.Join(rootDir, "etc", "systemd")
	if _, err := os.Stat(systemdDir); !os.IsNotExist(err) {
		t.Errorf("expected empty %s to be cleaned up, got err = %v", systemdDir, err)
	}
}

func TestEtcManager_PromoteStaticToEtc_WalksSymlinksInStaticDir(t *testing.T) {
	// The static dir itself is a symlink (created by SymlinkToStatic), and
	// the etc overlay inside it contains symlinks pointing into package
	// state dirs. PromoteStaticToEtc should follow symlinks in the static
	// tree to discover the leaf files.
	rootDir := t.TempDir()
	mgr := newEtcManager(rootDir)

	// Simulate a package state dir with a real file.
	pkgStateDir := filepath.Join(t.TempDir(), "containerd-abc123")
	os.MkdirAll(filepath.Join(pkgStateDir, "etc", "containerd"), 0755)
	os.WriteFile(filepath.Join(pkgStateDir, "etc", "containerd", "config.toml"), []byte("real-cfg"), 0644)

	// Create the etc overlay tree with a symlink (as etcOverlayPackage does).
	overlayDir := filepath.Join(t.TempDir(), "etc")
	os.MkdirAll(filepath.Join(overlayDir, "containerd"), 0755)
	os.Symlink(
		filepath.Join(pkgStateDir, "etc", "containerd", "config.toml"),
		filepath.Join(overlayDir, "containerd", "config.toml"),
	)

	if err := mgr.SymlinkToStatic(overlayDir); err != nil {
		t.Fatalf("SymlinkToStatic() error = %v", err)
	}
	if err := mgr.PromoteStaticToEtc(); err != nil {
		t.Fatalf("PromoteStaticToEtc() error = %v", err)
	}

	// The /etc/containerd/config.toml symlink should point through static.
	linkPath := filepath.Join(rootDir, "etc", "containerd", "config.toml")
	dest, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatalf("Readlink() error = %v", err)
	}
	wantTarget := filepath.Join(rootDir, "etc", "static", "containerd", "config.toml")
	if dest != wantTarget {
		t.Errorf("symlink target = %q, want %q", dest, wantTarget)
	}

	// And the full chain should resolve to the actual content.
	content, err := os.ReadFile(linkPath)
	if err != nil {
		t.Fatalf("reading through symlink chain: %v", err)
	}
	if string(content) != "real-cfg" {
		t.Errorf("content = %q, want %q", content, "real-cfg")
	}
}

func TestIsStaticSymlink(t *testing.T) {
	tests := []struct {
		name       string
		dest       string
		staticPath string
		want       bool
	}{
		{"inside static", "/root/etc/static/foo.conf", "/root/etc/static", true},
		{"nested inside static", "/root/etc/static/deep/nested/f", "/root/etc/static", true},
		{"outside static", "/some/other/path", "/root/etc/static", false},
		{"parent of static", "/root/etc", "/root/etc/static", false},
		{"static itself", "/root/etc/static", "/root/etc/static", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isStaticSymlink(tt.dest, tt.staticPath)
			if got != tt.want {
				t.Errorf("isStaticSymlink(%q, %q) = %v, want %v", tt.dest, tt.staticPath, got, tt.want)
			}
		})
	}
}
