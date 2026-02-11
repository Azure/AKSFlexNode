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
			PackageByNames: map[string]OverlayPackageDef{
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
	if err := overlay.prepareOverlayPackages(ctx); err != nil {
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
			PackageByNames: map[string]OverlayPackageDef{
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
	if err := overlay.prepareOverlayPackages(ctx); err != nil {
		t.Fatalf("first prepareOverlayPackages() error = %v", err)
	}

	// Record state dir contents.
	statesRoot := filepath.Join(root, statesDir)
	entries1, _ := os.ReadDir(statesRoot)

	// Second run: should skip (no new dirs, no errors).
	if err := overlay.prepareOverlayPackages(ctx); err != nil {
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
			PackageByNames: map[string]OverlayPackageDef{
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
	err := overlay.prepareOverlayPackages(ctx)
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

func TestPrepare_PersistsConfigAsJSON(t *testing.T) {
	root := t.TempDir()
	mgr := NewStoreManager(root)

	config := OverlayConfig{
		Version: "v1.2.3",
		PackageByNames: map[string]OverlayPackageDef{
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

	pkgDef, ok := got.PackageByNames["containerd"]
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
		PackageByNames: map[string]OverlayPackageDef{
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
		PackageByNames: map[string]OverlayPackageDef{
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

	if _, ok := got.PackageByNames["pkg2"]; !ok {
		t.Error("expected pkg2 in overwritten config")
	}
	if _, ok := got.PackageByNames["pkg1"]; ok {
		t.Error("expected pkg1 to be gone from overwritten config")
	}
}
