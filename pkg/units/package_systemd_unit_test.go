package units

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const kubeletUnitTemplate = `[Unit]
Description=Kubelet
After=network.target

[Service]
ExecStart=/usr/bin/kubelet
Restart=always

[Install]
WantedBy=multi-user.target
`

func TestNewSystemdUnitPackage(t *testing.T) {
	pkg := newSystemdUnitPackage("kubelet", "1.0.0", nil, kubeletUnitTemplate)
	if pkg.Name() != "kubelet" {
		t.Errorf("Name() = %q, want %q", pkg.Name(), "kubelet")
	}
	if pkg.Version() != "1.0.0" {
		t.Errorf("Version() = %q, want %q", pkg.Version(), "1.0.0")
	}
}

func TestSystemdUnitPackage_Install(t *testing.T) {
	pkg := newSystemdUnitPackage("kubelet", "1.0.0", nil, kubeletUnitTemplate)

	base := filepath.Join(t.TempDir(), "state")
	if err := pkg.Install(context.Background(), base); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	unitPath := filepath.Join(base, "kubelet.service")
	got, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("reading unit file: %v", err)
	}
	if string(got) != kubeletUnitTemplate {
		t.Errorf("unit file content = %q, want %q", got, kubeletUnitTemplate)
	}
}

func TestSystemdUnitPackage_EtcFiles(t *testing.T) {
	pkg := newSystemdUnitPackage("kubelet", "1.0.0", nil, kubeletUnitTemplate)

	etcFiles := pkg.EtcFiles()
	if len(etcFiles) != 1 {
		t.Fatalf("EtcFiles() returned %d entries, want 1", len(etcFiles))
	}

	wantSource := "kubelet.service"
	wantTarget := filepath.Join("systemd", "system", "kubelet.service")
	if etcFiles[0].Source != wantSource {
		t.Errorf("EtcFiles()[0].Source = %q, want %q", etcFiles[0].Source, wantSource)
	}
	if etcFiles[0].Target != wantTarget {
		t.Errorf("EtcFiles()[0].Target = %q, want %q", etcFiles[0].Target, wantTarget)
	}
}

func TestSystemdUnitPackage_Sources(t *testing.T) {
	depA := &installedPackage{Package: newSystemdUnitPackage("containerd", "1.0.0", nil, "dummy")}
	depB := &installedPackage{Package: newSystemdUnitPackage("kubelet-bin", "1.0.0", nil, "dummy")}

	pkg := newSystemdUnitPackage("kubelet", "1.0.0", []*installedPackage{depB, depA}, kubeletUnitTemplate)

	sources := pkg.Sources()
	if len(sources) != 2 {
		t.Fatalf("Sources() returned %d entries, want 2", len(sources))
	}

	// Sources should be sorted as <kind>://<name>.
	if sources[0] != "systemd-unit://containerd" {
		t.Errorf("Sources()[0] = %q, want %q", sources[0], "systemd-unit://containerd")
	}
	if sources[1] != "systemd-unit://kubelet-bin" {
		t.Errorf("Sources()[1] = %q, want %q", sources[1], "systemd-unit://kubelet-bin")
	}
}

func TestSystemdUnitPackage_Sources_NoPackages(t *testing.T) {
	pkg := newSystemdUnitPackage("kubelet", "1.0.0", nil, kubeletUnitTemplate)

	sources := pkg.Sources()
	if len(sources) != 0 {
		t.Fatalf("Sources() returned %d entries, want 0", len(sources))
	}
}

// newTestInstalledPackage creates an installedPackage with a real temp state dir.
// If withBin is true, a bin/ subdirectory is created inside the state dir.
func newTestInstalledPackage(t *testing.T, name string, withBin bool) *installedPackage {
	t.Helper()
	stateDir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatal(err)
	}
	if withBin {
		if err := os.MkdirAll(filepath.Join(stateDir, "bin"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	return &installedPackage{
		Package:            newSystemdUnitPackage(name, "1.0.0", nil, "dummy"),
		InstalledStatePath: stateDir,
	}
}

func TestSystemdUnitPackage_Install_TemplateGetPathEnv(t *testing.T) {
	depA := newTestInstalledPackage(t, "containerd", true)
	depB := newTestInstalledPackage(t, "kubelet-bin", true)

	tmpl := `[Service]
Environment="PATH={{ .GetPathEnv }}"
ExecStart=/usr/bin/kubelet
`
	pkg := newSystemdUnitPackage("kubelet", "1.0.0", []*installedPackage{depB, depA}, tmpl)

	base := filepath.Join(t.TempDir(), "state")
	if err := pkg.Install(context.Background(), base); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	got, err := os.ReadFile(filepath.Join(base, "kubelet.service"))
	if err != nil {
		t.Fatalf("reading unit file: %v", err)
	}

	content := string(got)
	// Both bin paths should appear, sorted and colon-separated.
	if !strings.Contains(content, filepath.Join(depA.InstalledStatePath, "bin")) {
		t.Errorf("expected containerd bin path in output, got:\n%s", content)
	}
	if !strings.Contains(content, filepath.Join(depB.InstalledStatePath, "bin")) {
		t.Errorf("expected kubelet-bin bin path in output, got:\n%s", content)
	}
	// GetPathEnv is hermetic — no system defaults.
	if strings.Contains(content, "/usr/local/sbin") {
		t.Errorf("expected no system default paths from GetPathEnv, got:\n%s", content)
	}
}

func TestSystemdUnitPackage_Install_TemplateGetPathEnv_NoBinDirs(t *testing.T) {
	dep := newTestInstalledPackage(t, "noBinPkg", false)

	tmpl := `PATH={{ .GetPathEnv }}`
	pkg := newSystemdUnitPackage("test", "1.0.0", []*installedPackage{dep}, tmpl)

	base := filepath.Join(t.TempDir(), "state")
	if err := pkg.Install(context.Background(), base); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	got, _ := os.ReadFile(filepath.Join(base, "test.service"))
	if string(got) != "PATH=" {
		t.Errorf("got %q, want %q", got, "PATH=")
	}
}

func TestSystemdUnitPackage_Install_TemplateGetPathEnvWithSystemDefaults(t *testing.T) {
	depA := newTestInstalledPackage(t, "containerd", true)

	tmpl := `PATH={{ .GetPathEnvWithSystemDefaults }}`
	pkg := newSystemdUnitPackage("test", "1.0.0", []*installedPackage{depA}, tmpl)

	base := filepath.Join(t.TempDir(), "state")
	if err := pkg.Install(context.Background(), base); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	got, _ := os.ReadFile(filepath.Join(base, "test.service"))
	content := string(got)
	if !strings.Contains(content, filepath.Join(depA.InstalledStatePath, "bin")) {
		t.Errorf("expected containerd bin path in output, got:\n%s", content)
	}
	if !strings.Contains(content, "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin") {
		t.Errorf("expected system default paths appended, got:\n%s", content)
	}
}

func TestSystemdUnitPackage_Install_TemplateGetPathEnvWithSystemDefaults_NoBinDirs(t *testing.T) {
	dep := newTestInstalledPackage(t, "noBinPkg", false)

	tmpl := `PATH={{ .GetPathEnvWithSystemDefaults }}`
	pkg := newSystemdUnitPackage("test", "1.0.0", []*installedPackage{dep}, tmpl)

	base := filepath.Join(t.TempDir(), "state")
	if err := pkg.Install(context.Background(), base); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	got, _ := os.ReadFile(filepath.Join(base, "test.service"))
	want := "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	if string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSystemdUnitPackage_Install_TemplateGetPackagePath(t *testing.T) {
	dep := newTestInstalledPackage(t, "kubelet-bin", true)

	tmpl := `ExecStart={{ .GetPackagePath "kubelet-bin" "bin" "kubelet" }}`
	pkg := newSystemdUnitPackage("kubelet", "1.0.0", []*installedPackage{dep}, tmpl)

	base := filepath.Join(t.TempDir(), "state")
	if err := pkg.Install(context.Background(), base); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	got, _ := os.ReadFile(filepath.Join(base, "kubelet.service"))
	want := "ExecStart=" + filepath.Join(dep.InstalledStatePath, "bin", "kubelet")
	if string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSystemdUnitPackage_Install_TemplateGetPackagePath_NotFound(t *testing.T) {
	dep := newTestInstalledPackage(t, "containerd", false)

	tmpl := `ExecStart={{ .GetPackagePath "nonexistent" "bin" "foo" }}`
	pkg := newSystemdUnitPackage("bad", "1.0.0", []*installedPackage{dep}, tmpl)

	base := filepath.Join(t.TempDir(), "state")
	err := pkg.Install(context.Background(), base)
	if err == nil {
		t.Fatal("expected error for unknown package reference in template")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error = %q, want it to mention the missing package", err)
	}
}
