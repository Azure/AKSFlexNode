package units

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSetupDiskLayout_CreatesRequiredDirs(t *testing.T) {
	root := t.TempDir()
	mgr := NewStoreManager(root)

	if err := mgr.setupDiskLayout(); err != nil {
		t.Fatalf("setupDiskLayout() error = %v", err)
	}

	for _, dir := range []string{
		root,
		filepath.Join(root, configsDir),
		filepath.Join(root, statesDir),
	} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Errorf("expected directory %s to exist, got error: %v", dir, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("expected %s to be a directory", dir)
		}
	}
}

// TestOverlay_Apply_StartsSystemdUnits verifies that the first Apply
// (no previous generation) starts all systemd units via the manager.
func TestOverlay_Apply_StartsSystemdUnits(t *testing.T) {
	root := t.TempDir()
	mgr := &fakeSystemdManager{}

	containerdSrc := filepath.Join(t.TempDir(), "containerd-src")
	os.MkdirAll(filepath.Join(containerdSrc, "bin"), 0755)
	os.WriteFile(filepath.Join(containerdSrc, "bin", "containerd"), []byte("containerd-bin"), 0755)

	overlay := NewOverlay(OverlayConfig{
		Version: "v1",
		PackagesByName: map[string]OverlayPackageDef{
			"containerd": {
				Version: "1.7.0",
				Source:  OverlayPackageSource{Type: overlayPackageSourceTypeFile, URI: containerdSrc},
			},
		},
		SystemdUnitsByName: map[string]OverlaySystemdUnitDef{
			"containerd": {
				Version:        "1.0.0",
				Packages:       []string{"containerd"},
				TemplateInline: "[Service]\nExecStart={{ .GetPackagePath \"containerd\" \"bin\" \"containerd\" }}",
			},
		},
	}, root, root, mgr)

	if err := overlay.Apply(context.Background()); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	// First apply: no previous generation, so the unit should be started.
	want := []string{
		"daemon-reload",
		"start:containerd.service",
	}
	if !reflect.DeepEqual(mgr.Actions, want) {
		t.Errorf("systemd actions = %v, want %v", mgr.Actions, want)
	}
}

// TestOverlay_Apply_TwoGenerations_SystemdDeltas verifies that applying
// a second generation correctly computes and applies systemd deltas:
// new units start, removed units stop, changed units restart.
func TestOverlay_Apply_TwoGenerations_SystemdDeltas(t *testing.T) {
	root := t.TempDir()

	// --- Generation 1: containerd + kubelet ---
	containerdSrc := filepath.Join(t.TempDir(), "containerd-src")
	os.MkdirAll(filepath.Join(containerdSrc, "bin"), 0755)
	os.WriteFile(filepath.Join(containerdSrc, "bin", "containerd"), []byte("containerd-v1"), 0755)

	kubeletSrc := filepath.Join(t.TempDir(), "kubelet-src")
	os.MkdirAll(filepath.Join(kubeletSrc, "bin"), 0755)
	os.WriteFile(filepath.Join(kubeletSrc, "bin", "kubelet"), []byte("kubelet-v1"), 0755)

	mgr1 := &fakeSystemdManager{}
	overlay1 := NewOverlay(OverlayConfig{
		Version: "v1",
		PackagesByName: map[string]OverlayPackageDef{
			"containerd": {
				Version: "1.0.0",
				Source:  OverlayPackageSource{Type: overlayPackageSourceTypeFile, URI: containerdSrc},
			},
			"kubelet": {
				Version: "1.0.0",
				Source:  OverlayPackageSource{Type: overlayPackageSourceTypeFile, URI: kubeletSrc},
			},
		},
		SystemdUnitsByName: map[string]OverlaySystemdUnitDef{
			"containerd": {
				Version:        "1.0.0",
				Packages:       []string{"containerd"},
				TemplateInline: "[Service]\nExecStart={{ .GetPackagePath \"containerd\" \"bin\" \"containerd\" }}",
			},
			"kubelet": {
				Version:        "1.0.0",
				Packages:       []string{"kubelet"},
				TemplateInline: "[Service]\nExecStart={{ .GetPackagePath \"kubelet\" \"bin\" \"kubelet\" }}",
			},
		},
	}, root, root, mgr1)

	ctx := context.Background()
	if err := overlay1.Apply(ctx); err != nil {
		t.Fatalf("gen1 Apply() error = %v", err)
	}

	// Gen1: both units should start.
	wantGen1 := []string{
		"daemon-reload",
		"start:containerd.service",
		"start:kubelet.service",
	}
	if !reflect.DeepEqual(mgr1.Actions, wantGen1) {
		t.Errorf("gen1 systemd actions = %v, want %v", mgr1.Actions, wantGen1)
	}

	// --- Generation 2: containerd (changed version), calico (new), kubelet removed ---
	containerdSrcV2 := filepath.Join(t.TempDir(), "containerd-src-v2")
	os.MkdirAll(filepath.Join(containerdSrcV2, "bin"), 0755)
	os.WriteFile(filepath.Join(containerdSrcV2, "bin", "containerd"), []byte("containerd-v2"), 0755)

	calicoSrc := filepath.Join(t.TempDir(), "calico-src")
	os.MkdirAll(filepath.Join(calicoSrc, "bin"), 0755)
	os.WriteFile(filepath.Join(calicoSrc, "bin", "calico-node"), []byte("calico-bin"), 0755)

	mgr2 := &fakeSystemdManager{}
	overlay2 := NewOverlay(OverlayConfig{
		Version: "v2",
		PackagesByName: map[string]OverlayPackageDef{
			"containerd": {
				Version: "2.0.0",
				Source:  OverlayPackageSource{Type: overlayPackageSourceTypeFile, URI: containerdSrcV2},
			},
			"calico": {
				Version: "3.26.0",
				Source:  OverlayPackageSource{Type: overlayPackageSourceTypeFile, URI: calicoSrc},
			},
		},
		SystemdUnitsByName: map[string]OverlaySystemdUnitDef{
			"containerd": {
				Version:        "2.0.0",
				Packages:       []string{"containerd"},
				TemplateInline: "[Service]\nExecStart={{ .GetPackagePath \"containerd\" \"bin\" \"containerd\" }}\nRestart=always",
			},
			"calico-node": {
				Version:        "1.0.0",
				Packages:       []string{"calico"},
				TemplateInline: "[Service]\nExecStart={{ .GetPackagePath \"calico\" \"bin\" \"calico-node\" }}",
			},
		},
	}, root, root, mgr2)

	if err := overlay2.Apply(ctx); err != nil {
		t.Fatalf("gen2 Apply() error = %v", err)
	}

	// Gen2: kubelet stopped, containerd restarted (changed), calico-node started.
	wantGen2 := []string{
		"stop:kubelet.service",
		"daemon-reload",
		"start:calico-node.service",
		"restart:containerd.service",
	}
	if !reflect.DeepEqual(mgr2.Actions, wantGen2) {
		t.Errorf("gen2 systemd actions = %v, want %v", mgr2.Actions, wantGen2)
	}
}

// TestOverlay_Apply_NoSystemdUnits_DaemonReload verifies that Apply with
// no systemd units still calls daemon-reload (and nothing else).
func TestOverlay_Apply_NoSystemdUnits_DaemonReload(t *testing.T) {
	root := t.TempDir()
	mgr := &fakeSystemdManager{}

	overlay := NewOverlay(OverlayConfig{
		Version: "v1",
	}, root, root, mgr)

	if err := overlay.Apply(context.Background()); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	want := []string{"daemon-reload"}
	if !reflect.DeepEqual(mgr.Actions, want) {
		t.Errorf("systemd actions = %v, want %v", mgr.Actions, want)
	}
}

// TestOverlay_Apply_UnchangedUnits_NoRestart verifies that applying the
// same overlay twice does not restart unchanged units.
func TestOverlay_Apply_UnchangedUnits_NoRestart(t *testing.T) {
	root := t.TempDir()

	containerdSrc := filepath.Join(t.TempDir(), "containerd-src")
	os.MkdirAll(filepath.Join(containerdSrc, "bin"), 0755)
	os.WriteFile(filepath.Join(containerdSrc, "bin", "containerd"), []byte("containerd-bin"), 0755)

	config := OverlayConfig{
		Version: "v1",
		PackagesByName: map[string]OverlayPackageDef{
			"containerd": {
				Version: "1.7.0",
				Source:  OverlayPackageSource{Type: overlayPackageSourceTypeFile, URI: containerdSrc},
			},
		},
		SystemdUnitsByName: map[string]OverlaySystemdUnitDef{
			"containerd": {
				Version:        "1.0.0",
				Packages:       []string{"containerd"},
				TemplateInline: "[Service]\nExecStart={{ .GetPackagePath \"containerd\" \"bin\" \"containerd\" }}",
			},
		},
	}

	// First Apply: starts the unit.
	mgr1 := &fakeSystemdManager{}
	overlay1 := NewOverlay(config, root, root, mgr1)

	ctx := context.Background()
	if err := overlay1.Apply(ctx); err != nil {
		t.Fatalf("first Apply() error = %v", err)
	}

	// Second Apply with same config: unchanged units should NOT be restarted.
	mgr2 := &fakeSystemdManager{}
	overlay2 := NewOverlay(config, root, root, mgr2)

	if err := overlay2.Apply(ctx); err != nil {
		t.Fatalf("second Apply() error = %v", err)
	}

	// Only daemon-reload expected — no start, stop, or restart.
	want := []string{"daemon-reload"}
	if !reflect.DeepEqual(mgr2.Actions, want) {
		t.Errorf("second apply systemd actions = %v, want %v", mgr2.Actions, want)
	}
}

func TestSetupDiskLayout_IsIdempotent(t *testing.T) {
	root := t.TempDir()
	mgr := NewStoreManager(root)

	if err := mgr.setupDiskLayout(); err != nil {
		t.Fatalf("first setupDiskLayout() error = %v", err)
	}
	if err := mgr.setupDiskLayout(); err != nil {
		t.Fatalf("second setupDiskLayout() error = %v", err)
	}
}

func TestNewStoreManager_DefaultRoot(t *testing.T) {
	mgr := NewStoreManager("")
	if mgr.root != DefaultStoreRoot {
		t.Errorf("expected root %q, got %q", DefaultStoreRoot, mgr.root)
	}
}

func TestNewStoreManager_CustomRoot(t *testing.T) {
	mgr := NewStoreManager("/custom/path")
	if mgr.root != "/custom/path" {
		t.Errorf("expected root %q, got %q", "/custom/path", mgr.root)
	}
}

func TestInstall_URL(t *testing.T) {
	content := []byte("binary-content-here")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	}))
	defer srv.Close()

	pkg, err := newOverlayPackage("testpkg", OverlayPackageDef{
		Version: "1.0.0",
		Source:  OverlayPackageSource{Type: overlayPackageSourceTypeURL, URI: srv.URL + "/testbin"},
	})
	if err != nil {
		t.Fatalf("newOverlayPackage() error = %v", err)
	}

	base := filepath.Join(t.TempDir(), "pkg-out")
	if err := pkg.Install(context.Background(), base); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	got, err := os.ReadFile(filepath.Join(base, "testbin"))
	if err != nil {
		t.Fatalf("reading installed file: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("installed file content = %q, want %q", got, content)
	}
}

func TestInstall_URLTar(t *testing.T) {
	// Build a gzipped tarball in memory with two files.
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	files := map[string]string{
		"bin/containerd":      "containerd-binary",
		"bin/containerd-shim": "shim-binary",
	}
	for name, body := range files {
		hdr := &tar.Header{
			Name: name,
			Mode: 0755,
			Size: int64(len(body)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("writing tar header: %v", err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("writing tar body: %v", err)
		}
	}
	tw.Close()
	gw.Close()

	tarball := buf.Bytes()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(tarball)
	}))
	defer srv.Close()

	pkg, err := newOverlayPackage("containerd", OverlayPackageDef{
		Version: "1.6.21",
		Source:  OverlayPackageSource{Type: overlayPackageSourceTypeURLTar, URI: srv.URL + "/containerd.tar.gz"},
	})
	if err != nil {
		t.Fatalf("newOverlayPackage() error = %v", err)
	}

	base := filepath.Join(t.TempDir(), "pkg-out")
	if err := pkg.Install(context.Background(), base); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	for name, wantBody := range files {
		got, err := os.ReadFile(filepath.Join(base, name))
		if err != nil {
			t.Errorf("reading %s: %v", name, err)
			continue
		}
		if string(got) != wantBody {
			t.Errorf("file %s content = %q, want %q", name, got, wantBody)
		}
	}
}

func TestInstall_URLZip(t *testing.T) {
	// Build a zip archive in memory.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	files := map[string]string{
		"bin/kubelet": "kubelet-binary",
		"bin/kubectl": "kubectl-binary",
	}
	for name, body := range files {
		fw, err := zw.Create(name)
		if err != nil {
			t.Fatalf("creating zip entry: %v", err)
		}
		if _, err := fw.Write([]byte(body)); err != nil {
			t.Fatalf("writing zip entry: %v", err)
		}
	}
	zw.Close()

	zipData := buf.Bytes()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(zipData)
	}))
	defer srv.Close()

	pkg, err := newOverlayPackage("kubelet", OverlayPackageDef{
		Version: "1.28.0",
		Source:  OverlayPackageSource{Type: overlayPackageSourceTypeURLZip, URI: srv.URL + "/kubelet.zip"},
	})
	if err != nil {
		t.Fatalf("newOverlayPackage() error = %v", err)
	}

	base := filepath.Join(t.TempDir(), "pkg-out")
	if err := pkg.Install(context.Background(), base); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	for name, wantBody := range files {
		got, err := os.ReadFile(filepath.Join(base, name))
		if err != nil {
			t.Errorf("reading %s: %v", name, err)
			continue
		}
		if string(got) != wantBody {
			t.Errorf("file %s content = %q, want %q", name, got, wantBody)
		}
	}
}

func TestInstall_File(t *testing.T) {
	// Create a local source file.
	srcDir := t.TempDir()
	srcFile := filepath.Join(srcDir, "runc")
	if err := os.WriteFile(srcFile, []byte("runc-binary"), 0644); err != nil {
		t.Fatalf("creating source file: %v", err)
	}

	pkg, err := newOverlayPackage("runc", OverlayPackageDef{
		Version: "1.1.4",
		Source:  OverlayPackageSource{Type: overlayPackageSourceTypeFile, URI: srcFile},
	})
	if err != nil {
		t.Fatalf("newOverlayPackage() error = %v", err)
	}

	base := filepath.Join(t.TempDir(), "pkg-out")
	if err := pkg.Install(context.Background(), base); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	got, err := os.ReadFile(filepath.Join(base, "runc"))
	if err != nil {
		t.Fatalf("reading installed file: %v", err)
	}
	if string(got) != "runc-binary" {
		t.Errorf("installed file content = %q, want %q", got, "runc-binary")
	}
}

func TestInstall_FileDirectory(t *testing.T) {
	// Create a source directory tree.
	srcDir := filepath.Join(t.TempDir(), "containerd-pkg")
	os.MkdirAll(filepath.Join(srcDir, "bin"), 0755)
	os.MkdirAll(filepath.Join(srcDir, "etc", "containerd"), 0755)
	os.WriteFile(filepath.Join(srcDir, "bin", "containerd"), []byte("containerd-bin"), 0755)
	os.WriteFile(filepath.Join(srcDir, "bin", "containerd-shim"), []byte("shim-bin"), 0755)
	os.WriteFile(filepath.Join(srcDir, "etc", "containerd", "config.toml"), []byte("cfg"), 0644)

	pkg, err := newOverlayPackage("containerd", OverlayPackageDef{
		Version: "1.6.21",
		Source:  OverlayPackageSource{Type: overlayPackageSourceTypeFile, URI: srcDir},
	})
	if err != nil {
		t.Fatalf("newOverlayPackage() error = %v", err)
	}

	base := filepath.Join(t.TempDir(), "pkg-out")
	if err := pkg.Install(context.Background(), base); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	want := map[string]string{
		"bin/containerd":             "containerd-bin",
		"bin/containerd-shim":        "shim-bin",
		"etc/containerd/config.toml": "cfg",
	}
	for name, wantBody := range want {
		got, err := os.ReadFile(filepath.Join(base, name))
		if err != nil {
			t.Errorf("reading %s: %v", name, err)
			continue
		}
		if string(got) != wantBody {
			t.Errorf("file %s content = %q, want %q", name, got, wantBody)
		}
	}
}

func TestInstall_CreatesBaseDir(t *testing.T) {
	srcDir := t.TempDir()
	srcFile := filepath.Join(srcDir, "bin")
	if err := os.WriteFile(srcFile, []byte("data"), 0644); err != nil {
		t.Fatalf("creating source file: %v", err)
	}

	pkg, err := newOverlayPackage("test", OverlayPackageDef{
		Version: "1.0.0",
		Source:  OverlayPackageSource{Type: overlayPackageSourceTypeFile, URI: srcFile},
	})
	if err != nil {
		t.Fatalf("newOverlayPackage() error = %v", err)
	}

	// base does not exist yet — Install should create it.
	base := filepath.Join(t.TempDir(), "nested", "deep", "pkg-out")
	if err := pkg.Install(context.Background(), base); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	if _, err := os.Stat(base); err != nil {
		t.Errorf("expected base dir %s to exist: %v", base, err)
	}
}

func TestInstall_URLTar_PathTraversal(t *testing.T) {
	// Build a tarball with a malicious path.
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	hdr := &tar.Header{
		Name: "../../etc/passwd",
		Mode: 0644,
		Size: 5,
	}
	tw.WriteHeader(hdr)
	tw.Write([]byte("pwned"))
	tw.Close()
	gw.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(buf.Bytes())
	}))
	defer srv.Close()

	pkg, err := newOverlayPackage("evil", OverlayPackageDef{
		Version: "1.0.0",
		Source:  OverlayPackageSource{Type: overlayPackageSourceTypeURLTar, URI: srv.URL + "/evil.tar.gz"},
	})
	if err != nil {
		t.Fatalf("newOverlayPackage() error = %v", err)
	}

	base := filepath.Join(t.TempDir(), "pkg-out")
	err = pkg.Install(context.Background(), base)
	if err == nil {
		t.Fatal("Install() should have failed for path traversal")
	}
}

func TestPrepareOverlayPackages_InstallsToStateDir(t *testing.T) {
	root := t.TempDir()
	mgr := NewStoreManager(root)
	if err := mgr.setupDiskLayout(); err != nil {
		t.Fatalf("setupDiskLayout() error = %v", err)
	}

	// Create local source files for two packages.
	srcA := filepath.Join(t.TempDir(), "binaryA")
	os.WriteFile(srcA, []byte("aaa"), 0755)
	srcB := filepath.Join(t.TempDir(), "binaryB")
	os.WriteFile(srcB, []byte("bbb"), 0755)

	overlay := &Overlay{
		config: OverlayConfig{
			PackagesByName: map[string]OverlayPackageDef{
				"pkgA": {
					Version: "1.0.0",
					Source: struct {
						Type string `json:"type"`
						URI  string `json:"uri"`
					}{Type: overlayPackageSourceTypeFile, URI: srcA},
				},
				"pkgB": {
					Version: "2.0.0",
					Source: struct {
						Type string `json:"type"`
						URI  string `json:"uri"`
					}{Type: overlayPackageSourceTypeFile, URI: srcB},
				},
			},
		},
		store: mgr,
	}

	ctx := context.Background()
	if _, err := overlay.prepareOverlayPackages(ctx); err != nil {
		t.Fatalf("prepareOverlayPackages() error = %v", err)
	}

	// Verify state directories were created for both packages.
	statesRoot := filepath.Join(root, statesDir)
	entries, err := os.ReadDir(statesRoot)
	if err != nil {
		t.Fatalf("reading states dir: %v", err)
	}

	foundA, foundB := false, false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "pkgA-") {
			foundA = true
		}
		if strings.HasPrefix(e.Name(), "pkgB-") {
			foundB = true
		}
	}
	if !foundA {
		t.Error("expected state dir for pkgA")
	}
	if !foundB {
		t.Error("expected state dir for pkgB")
	}
}

func TestPreparePackages_SkipsCachedPackage(t *testing.T) {
	root := t.TempDir()
	mgr := NewStoreManager(root)
	if err := mgr.setupDiskLayout(); err != nil {
		t.Fatalf("setupDiskLayout() error = %v", err)
	}

	srcFile := filepath.Join(t.TempDir(), "binary")
	os.WriteFile(srcFile, []byte("data"), 0755)

	overlay := &Overlay{
		config: OverlayConfig{
			PackagesByName: map[string]OverlayPackageDef{
				"mypkg": {
					Version: "1.0.0",
					Source: struct {
						Type string `json:"type"`
						URI  string `json:"uri"`
					}{Type: overlayPackageSourceTypeFile, URI: srcFile},
				},
			},
		},
		store: mgr,
	}

	ctx := context.Background()

	// First run: installs the package.
	if _, err := overlay.prepareOverlayPackages(ctx); err != nil {
		t.Fatalf("first prepareOverlayPackages() error = %v", err)
	}

	// Record state dir contents.
	statesRoot := filepath.Join(root, statesDir)
	entries1, _ := os.ReadDir(statesRoot)

	// Second run: should skip (no new dirs, no errors).
	if _, err := overlay.prepareOverlayPackages(ctx); err != nil {
		t.Fatalf("second prepareOverlayPackages() error = %v", err)
	}

	entries2, _ := os.ReadDir(statesRoot)
	if len(entries1) != len(entries2) {
		t.Errorf("expected same number of state dirs after second run: got %d, want %d", len(entries2), len(entries1))
	}
}

func TestPrepareOverlayPackages_CleansUpOnFailure(t *testing.T) {
	root := t.TempDir()
	mgr := NewStoreManager(root)
	if err := mgr.setupDiskLayout(); err != nil {
		t.Fatalf("setupDiskLayout() error = %v", err)
	}

	overlay := &Overlay{
		config: OverlayConfig{
			PackagesByName: map[string]OverlayPackageDef{
				"badpkg": {
					Version: "1.0.0",
					Source: struct {
						Type string `json:"type"`
						URI  string `json:"uri"`
					}{Type: overlayPackageSourceTypeFile, URI: "/nonexistent/path/to/file"},
				},
			},
		},
		store: mgr,
	}

	ctx := context.Background()
	_, err := overlay.prepareOverlayPackages(ctx)
	if err == nil {
		t.Fatal("expected preparePackages() to fail")
	}

	// No leftover temp dirs should remain in states.
	statesRoot := filepath.Join(root, statesDir)
	entries, _ := os.ReadDir(statesRoot)
	for _, e := range entries {
		t.Errorf("unexpected leftover entry in states dir: %s", e.Name())
	}
}

// TestEtcOverlay_SymlinksValidAfterAtomicRename verifies that symlinks created
// by etcOverlayPackage still resolve correctly after installPackage atomically
// renames the temp directory to the final state directory. The symlink targets
// are absolute paths to other packages' state dirs, so renaming the directory
// containing the symlinks must not invalidate them.
func TestEtcOverlay_SymlinksValidAfterAtomicRename(t *testing.T) {
	root := t.TempDir()
	mgr := NewStoreManager(root)
	if err := mgr.setupDiskLayout(); err != nil {
		t.Fatalf("setupDiskLayout() error = %v", err)
	}

	// Create a real source directory for a dependency package with files
	// that will be symlinked from the etc overlay.
	depSrcDir := filepath.Join(t.TempDir(), "containerd-src")
	os.MkdirAll(filepath.Join(depSrcDir, "bin"), 0755)
	os.MkdirAll(filepath.Join(depSrcDir, "etc", "containerd"), 0755)
	os.WriteFile(filepath.Join(depSrcDir, "bin", "containerd"), []byte("containerd-binary"), 0755)
	os.WriteFile(filepath.Join(depSrcDir, "etc", "containerd", "config.toml"), []byte("root = \"/var/lib/containerd\""), 0644)

	depPkg, err := newOverlayPackage("containerd", OverlayPackageDef{
		Version: "1.7.0",
		Source:  OverlayPackageSource{Type: overlayPackageSourceTypeFile, URI: depSrcDir},
		ETCFiles: []PackageEtcFile{
			{Source: "etc/containerd/config.toml", Target: "containerd/config.toml"},
		},
	})
	if err != nil {
		t.Fatalf("newOverlayPackage() error = %v", err)
	}

	// Install the dependency package through installPackage (temp dir + atomic rename).
	depStateDir, err := mgr.installPackage(context.Background(), depPkg)
	if err != nil {
		t.Fatalf("installPackage(containerd) error = %v", err)
	}

	// Create the etc overlay referencing the installed dependency.
	installedDep := &InstalledPackage{
		Package:            depPkg,
		InstalledStatePath: depStateDir,
	}
	etcPkg := newEtcOverlayPackage("v1", []*InstalledPackage{installedDep})

	// Install the etc overlay itself through installPackage (another temp dir + atomic rename).
	etcStateDir, err := mgr.installPackage(context.Background(), etcPkg)
	if err != nil {
		t.Fatalf("installPackage(etc) error = %v", err)
	}

	// The etc state dir should NOT be the temp dir — it should be the final fingerprinted path.
	if strings.Contains(filepath.Base(etcStateDir), "-tmp-") {
		t.Errorf("etc state dir looks like a temp dir: %s", etcStateDir)
	}

	// Verify the symlink exists and points to the dependency's final state dir.
	symlinkPath := filepath.Join(etcStateDir, "etc", "containerd", "config.toml")
	linkTarget, err := os.Readlink(symlinkPath)
	if err != nil {
		t.Fatalf("Readlink(%q) error = %v", symlinkPath, err)
	}

	expectedTarget := filepath.Join(depStateDir, "etc", "containerd", "config.toml")
	if linkTarget != expectedTarget {
		t.Errorf("symlink target = %q, want %q", linkTarget, expectedTarget)
	}

	// The critical check: does the symlink actually resolve to the correct content?
	// This proves the symlink survived the atomic rename of its own directory.
	content, err := os.ReadFile(symlinkPath)
	if err != nil {
		t.Fatalf("reading through symlink after atomic rename: %v", err)
	}
	if string(content) != "root = \"/var/lib/containerd\"" {
		t.Errorf("content through symlink = %q, want %q", content, "root = \"/var/lib/containerd\"")
	}
}

// TestPrepareEtcOverlay_EndToEnd exercises the full Overlay.Prepare flow with
// real file-based packages that declare EtcFiles and a systemd unit, then
// verifies that the etc overlay's symlinks resolve correctly after all the
// atomic renames have completed.
func TestPrepareEtcOverlay_EndToEnd(t *testing.T) {
	root := t.TempDir()
	mgr := NewStoreManager(root)

	// Create source directories for two packages.
	containerdSrc := filepath.Join(t.TempDir(), "containerd-src")
	os.MkdirAll(filepath.Join(containerdSrc, "bin"), 0755)
	os.MkdirAll(filepath.Join(containerdSrc, "etc", "containerd"), 0755)
	os.WriteFile(filepath.Join(containerdSrc, "bin", "containerd"), []byte("containerd-bin"), 0755)
	os.WriteFile(filepath.Join(containerdSrc, "etc", "containerd", "config.toml"), []byte("cfg-content"), 0644)

	runcSrc := filepath.Join(t.TempDir(), "runc-src")
	os.MkdirAll(filepath.Join(runcSrc, "bin"), 0755)
	os.WriteFile(filepath.Join(runcSrc, "bin", "runc"), []byte("runc-bin"), 0755)

	overlay := &Overlay{
		config: OverlayConfig{
			Version: "v1-test",
			PackagesByName: map[string]OverlayPackageDef{
				"containerd": {
					Version: "1.7.0",
					Source:  OverlayPackageSource{Type: overlayPackageSourceTypeFile, URI: containerdSrc},
					ETCFiles: []PackageEtcFile{
						{Source: "etc/containerd/config.toml", Target: "containerd/config.toml"},
					},
				},
				"runc": {
					Version: "1.1.4",
					Source:  OverlayPackageSource{Type: overlayPackageSourceTypeFile, URI: runcSrc},
					// runc has no etc files — should be fine.
				},
			},
			SystemdUnitsByName: map[string]OverlaySystemdUnitDef{
				"containerd": {
					Version:        "1.0.0",
					Packages:       []string{"containerd", "runc"},
					TemplateInline: "[Service]\nExecStart={{ .GetPackagePath \"containerd\" \"bin\" \"containerd\" }}",
				},
			},
		},
		store: mgr,
	}

	ctx := context.Background()
	etcOverlay, err := overlay.Prepare(ctx)
	if err != nil {
		t.Fatalf("Overlay.Prepare() error = %v", err)
	}
	if etcOverlay == nil {
		t.Fatal("Overlay.Prepare() returned nil etc overlay")
	}

	etcStateDir := etcOverlay.InstalledStatePath

	// Verify the containerd config symlink resolves to the correct content.
	containerdCfg := filepath.Join(etcStateDir, "etc", "containerd", "config.toml")
	content, err := os.ReadFile(containerdCfg)
	if err != nil {
		t.Fatalf("reading containerd config through etc overlay symlink: %v", err)
	}
	if string(content) != "cfg-content" {
		t.Errorf("containerd config content = %q, want %q", content, "cfg-content")
	}

	// Verify the systemd unit symlink resolves to a rendered template.
	unitFile := filepath.Join(etcStateDir, "etc", "systemd", "system", "containerd.service")
	unitContent, err := os.ReadFile(unitFile)
	if err != nil {
		t.Fatalf("reading containerd.service through etc overlay symlink: %v", err)
	}
	// The rendered template should contain the actual installed path to containerd binary.
	if !strings.Contains(string(unitContent), "/bin/containerd") {
		t.Errorf("containerd.service content = %q, expected it to contain the resolved containerd binary path", unitContent)
	}

	// Verify the symlinks are actual symlinks (not copies).
	linkTarget, err := os.Readlink(containerdCfg)
	if err != nil {
		t.Fatalf("Readlink() error = %v", err)
	}
	// The target should point into the containerd package's state dir, not the etc dir.
	if !strings.Contains(linkTarget, "containerd-") {
		t.Errorf("symlink target %q should point into the containerd package state dir", linkTarget)
	}
}

func TestPrepare_PersistsConfigAsJSON(t *testing.T) {
	root := t.TempDir()
	mgr := NewStoreManager(root)

	config := OverlayConfig{
		Version: "v1.2.3",
		PackagesByName: map[string]OverlayPackageDef{
			"containerd": {
				Version: "1.6.21",
				Source: struct {
					Type string `json:"type"`
					URI  string `json:"uri"`
				}{Type: "url+tar", URI: "https://example.com/containerd.tar.gz"},
				ETCFiles: []PackageEtcFile{
					{Source: "etc/containerd/config.toml", Target: "containerd/config.toml"},
				},
			},
		},
	}

	if err := mgr.Prepare(config); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	// Verify the config file was created.
	configPath := filepath.Join(root, configsDir, "v1.2.3.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading config file: %v", err)
	}

	// Verify it round-trips back to the same config.
	var got OverlayConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshaling config: %v", err)
	}

	if got.Version != config.Version {
		t.Errorf("version = %q, want %q", got.Version, config.Version)
	}

	pkgDef, ok := got.PackagesByName["containerd"]
	if !ok {
		t.Fatal("expected containerd package in deserialized config")
	}
	if pkgDef.Version != "1.6.21" {
		t.Errorf("containerd version = %q, want %q", pkgDef.Version, "1.6.21")
	}
	if pkgDef.Source.Type != "url+tar" {
		t.Errorf("containerd source type = %q, want %q", pkgDef.Source.Type, "url+tar")
	}
	if pkgDef.Source.URI != "https://example.com/containerd.tar.gz" {
		t.Errorf("containerd source URI = %q, want %q", pkgDef.Source.URI, "https://example.com/containerd.tar.gz")
	}
	if len(pkgDef.ETCFiles) != 1 || pkgDef.ETCFiles[0].Source != "etc/containerd/config.toml" {
		t.Errorf("containerd etc files = %+v, want one entry with source etc/containerd/config.toml", pkgDef.ETCFiles)
	}
}

func TestPrepare_RequiresVersion(t *testing.T) {
	root := t.TempDir()
	mgr := NewStoreManager(root)

	err := mgr.Prepare(OverlayConfig{})
	if err == nil {
		t.Fatal("Prepare() should fail when version is empty")
	}
	if !strings.Contains(err.Error(), "version is required") {
		t.Errorf("error = %q, want it to mention version is required", err)
	}
}

func TestPrepare_OverwritesExistingConfig(t *testing.T) {
	root := t.TempDir()
	mgr := NewStoreManager(root)

	config1 := OverlayConfig{
		Version: "v1",
		PackagesByName: map[string]OverlayPackageDef{
			"pkg1": {
				Version: "1.0.0",
				Source: struct {
					Type string `json:"type"`
					URI  string `json:"uri"`
				}{Type: "file", URI: "/some/path"},
			},
		},
	}

	config2 := OverlayConfig{
		Version: "v1",
		PackagesByName: map[string]OverlayPackageDef{
			"pkg2": {
				Version: "2.0.0",
				Source: struct {
					Type string `json:"type"`
					URI  string `json:"uri"`
				}{Type: "url", URI: "https://example.com/pkg2"},
			},
		},
	}

	if err := mgr.Prepare(config1); err != nil {
		t.Fatalf("first Prepare() error = %v", err)
	}
	if err := mgr.Prepare(config2); err != nil {
		t.Fatalf("second Prepare() error = %v", err)
	}

	configPath := filepath.Join(root, configsDir, "v1.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading config file: %v", err)
	}

	var got OverlayConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshaling config: %v", err)
	}

	if _, ok := got.PackagesByName["pkg2"]; !ok {
		t.Error("expected pkg2 in overwritten config")
	}
	if _, ok := got.PackagesByName["pkg1"]; ok {
		t.Error("expected pkg1 to be gone from overwritten config")
	}
}

func TestNewOverlay_DefaultRoot(t *testing.T) {
	o := NewOverlay(OverlayConfig{Version: "v1"}, "", "", nil)
	if o.store.root != DefaultStoreRoot {
		t.Errorf("store root = %q, want %q", o.store.root, DefaultStoreRoot)
	}
	if o.etc.rootDir != "/" {
		t.Errorf("etc rootDir = %q, want %q", o.etc.rootDir, "/")
	}
}

func TestNewOverlay_CustomRoot(t *testing.T) {
	o := NewOverlay(OverlayConfig{Version: "v1"}, "/custom/store", "/custom/etc", nil)
	if o.store.root != "/custom/store" {
		t.Errorf("store root = %q, want %q", o.store.root, "/custom/store")
	}
	if o.etc.rootDir != "/custom/etc" {
		t.Errorf("etc rootDir = %q, want %q", o.etc.rootDir, "/custom/etc")
	}
}

func TestOverlay_Apply_WithoutPrepare(t *testing.T) {
	root := t.TempDir()
	o := NewOverlay(OverlayConfig{Version: "v1"}, root, root, nil)

	// Apply now calls Prepare internally, so an empty config with a valid
	// version should succeed (empty overlay, no packages).
	err := o.Apply(context.Background())
	if err != nil {
		t.Fatalf("Apply() with empty config should succeed, got error = %v", err)
	}
}

// TestOverlay_Apply_EndToEnd exercises the full Prepare → Apply pipeline:
// packages are installed, the etc overlay is built, then Apply wires up
// /etc/static and promotes entries into /etc.
func TestOverlay_Apply_EndToEnd(t *testing.T) {
	root := t.TempDir()

	// Create source directory for a package with an etc file.
	containerdSrc := filepath.Join(t.TempDir(), "containerd-src")
	os.MkdirAll(filepath.Join(containerdSrc, "bin"), 0755)
	os.MkdirAll(filepath.Join(containerdSrc, "etc", "containerd"), 0755)
	os.WriteFile(filepath.Join(containerdSrc, "bin", "containerd"), []byte("containerd-bin"), 0755)
	os.WriteFile(filepath.Join(containerdSrc, "etc", "containerd", "config.toml"), []byte("root = \"/var/lib/containerd\""), 0644)

	overlay := NewOverlay(OverlayConfig{
		Version: "v1-apply-test",
		PackagesByName: map[string]OverlayPackageDef{
			"containerd": {
				Version: "1.7.0",
				Source:  OverlayPackageSource{Type: overlayPackageSourceTypeFile, URI: containerdSrc},
				ETCFiles: []PackageEtcFile{
					{Source: "etc/containerd/config.toml", Target: "containerd/config.toml"},
				},
			},
		},
		SystemdUnitsByName: map[string]OverlaySystemdUnitDef{
			"containerd": {
				Version:        "1.0.0",
				Packages:       []string{"containerd"},
				TemplateInline: "[Service]\nExecStart={{ .GetPackagePath \"containerd\" \"bin\" \"containerd\" }}",
			},
		},
	}, root, root, nil)

	ctx := context.Background()

	// Prepare must succeed.
	etcOverlay, err := overlay.Prepare(ctx)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if etcOverlay == nil {
		t.Fatal("Prepare() returned nil etc overlay")
	}

	// Apply must succeed.
	if err := overlay.Apply(ctx); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	// Verify /etc/static symlink exists and points to the etc overlay's etc/ tree.
	staticLink := filepath.Join(root, "etc", "static")
	linkTarget, err := os.Readlink(staticLink)
	if err != nil {
		t.Fatalf("Readlink(etc/static) error = %v", err)
	}
	expectedTarget := filepath.Join(etcOverlay.InstalledStatePath, "etc")
	if linkTarget != expectedTarget {
		t.Errorf("etc/static -> %q, want %q", linkTarget, expectedTarget)
	}

	// Verify the promoted symlink for containerd config exists.
	promotedCfg := filepath.Join(root, "etc", "containerd", "config.toml")
	cfgTarget, err := os.Readlink(promotedCfg)
	if err != nil {
		t.Fatalf("Readlink(etc/containerd/config.toml) error = %v", err)
	}
	expectedCfgTarget := filepath.Join(etcOverlay.InstalledStatePath, "etc", "containerd", "config.toml")
	if cfgTarget != expectedCfgTarget {
		t.Errorf("etc/containerd/config.toml -> %q, want %q", cfgTarget, expectedCfgTarget)
	}

	// Verify the content resolves end-to-end.
	content, err := os.ReadFile(promotedCfg)
	if err != nil {
		t.Fatalf("reading promoted config: %v", err)
	}
	if string(content) != "root = \"/var/lib/containerd\"" {
		t.Errorf("config content = %q, want %q", content, "root = \"/var/lib/containerd\"")
	}

	// Verify the promoted symlink for the systemd unit exists.
	promotedUnit := filepath.Join(root, "etc", "systemd", "system", "containerd.service")
	unitContent, err := os.ReadFile(promotedUnit)
	if err != nil {
		t.Fatalf("reading promoted containerd.service: %v", err)
	}
	if !strings.Contains(string(unitContent), "/bin/containerd") {
		t.Errorf("containerd.service content = %q, expected it to contain the resolved containerd binary path", unitContent)
	}
}

// TestOverlay_Apply_TwoGenerations verifies that applying a second generation
// correctly replaces the first: new entries appear, stale entries are removed.
func TestOverlay_Apply_TwoGenerations(t *testing.T) {
	root := t.TempDir()

	// --- Generation 1: containerd with config ---
	containerdSrc := filepath.Join(t.TempDir(), "containerd-src")
	os.MkdirAll(filepath.Join(containerdSrc, "etc", "containerd"), 0755)
	os.WriteFile(filepath.Join(containerdSrc, "etc", "containerd", "config.toml"), []byte("gen1"), 0644)

	overlay1 := NewOverlay(OverlayConfig{
		Version: "v1",
		PackagesByName: map[string]OverlayPackageDef{
			"containerd": {
				Version: "1.0.0",
				Source:  OverlayPackageSource{Type: overlayPackageSourceTypeFile, URI: containerdSrc},
				ETCFiles: []PackageEtcFile{
					{Source: "etc/containerd/config.toml", Target: "containerd/config.toml"},
				},
			},
		},
	}, root, root, nil)

	ctx := context.Background()
	if _, err := overlay1.Prepare(ctx); err != nil {
		t.Fatalf("gen1 Prepare() error = %v", err)
	}
	if err := overlay1.Apply(ctx); err != nil {
		t.Fatalf("gen1 Apply() error = %v", err)
	}

	// Verify gen1 entry exists.
	gen1Cfg := filepath.Join(root, "etc", "containerd", "config.toml")
	if content, err := os.ReadFile(gen1Cfg); err != nil {
		t.Fatalf("gen1: reading promoted config: %v", err)
	} else if string(content) != "gen1" {
		t.Errorf("gen1 config = %q, want %q", content, "gen1")
	}

	// --- Generation 2: kubelet replaces containerd ---
	kubeletSrc := filepath.Join(t.TempDir(), "kubelet-src")
	os.MkdirAll(filepath.Join(kubeletSrc, "etc", "kubernetes"), 0755)
	os.WriteFile(filepath.Join(kubeletSrc, "etc", "kubernetes", "kubelet.conf"), []byte("gen2"), 0644)

	overlay2 := NewOverlay(OverlayConfig{
		Version: "v2",
		PackagesByName: map[string]OverlayPackageDef{
			"kubelet": {
				Version: "1.28.0",
				Source:  OverlayPackageSource{Type: overlayPackageSourceTypeFile, URI: kubeletSrc},
				ETCFiles: []PackageEtcFile{
					{Source: "etc/kubernetes/kubelet.conf", Target: "kubernetes/kubelet.conf"},
				},
			},
		},
	}, root, root, nil)

	if _, err := overlay2.Prepare(ctx); err != nil {
		t.Fatalf("gen2 Prepare() error = %v", err)
	}
	if err := overlay2.Apply(ctx); err != nil {
		t.Fatalf("gen2 Apply() error = %v", err)
	}

	// Verify gen2 entry exists.
	gen2Cfg := filepath.Join(root, "etc", "kubernetes", "kubelet.conf")
	if content, err := os.ReadFile(gen2Cfg); err != nil {
		t.Fatalf("gen2: reading promoted config: %v", err)
	} else if string(content) != "gen2" {
		t.Errorf("gen2 config = %q, want %q", content, "gen2")
	}

	// Verify gen1 entry was cleaned up (symlink removed).
	if _, err := os.Lstat(gen1Cfg); !os.IsNotExist(err) {
		t.Errorf("gen1 containerd/config.toml should have been removed, but err = %v", err)
	}

	// Verify the containerd parent dir was cleaned up (empty).
	if _, err := os.Stat(filepath.Join(root, "etc", "containerd")); !os.IsNotExist(err) {
		t.Errorf("gen1 etc/containerd/ dir should have been removed, but err = %v", err)
	}
}
