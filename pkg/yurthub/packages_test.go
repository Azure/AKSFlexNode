package yurthub

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func zstdBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	zw, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer zw.Close()
	return zw.EncodeAll(data, nil)
}

// testDeb builds a minimal .deb with name in data.tar.zst.
func testDeb(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	for _, f := range []struct {
		name string
		data []byte
	}{{"./usr/share/doc/yurthub/copyright", []byte("doc")}, {"./" + name, content}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var deb bytes.Buffer
	deb.WriteString("!<arch>\n")
	for _, m := range []struct {
		name string
		data []byte
	}{{"debian-binary", []byte("2.0\n")}, {"control.tar.zst", zstdBytes(t, []byte("c"))}, {"data.tar.zst", zstdBytes(t, tarBuf.Bytes())}} {
		fmt.Fprintf(&deb, "%-16s%-12s%-6s%-6s%-8s%-10d`\n", m.name, "0", "0", "0", "100644", len(m.data))
		deb.Write(m.data)
		if len(m.data)%2 == 1 {
			deb.WriteByte('\n')
		}
	}
	return deb.Bytes()
}

// testRPM builds a minimal .rpm whose zstd cpio payload holds name.
func testRPM(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var cpio bytes.Buffer
	entry := func(name string, mode int, data []byte) {
		fmt.Fprintf(&cpio, "070701%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x", 0, mode, 0, 0, 1, 0, len(data), 0, 0, 0, 0, len(name)+1, 0)
		cpio.WriteString(name + "\x00")
		cpio.Write(make([]byte, (4-(110+len(name)+1)%4)%4))
		cpio.Write(data)
		cpio.Write(make([]byte, (4-len(data)%4)%4))
	}
	entry("./usr/bin", 0o40755, nil)
	entry("./"+name, 0o100755, content)
	entry("TRAILER!!!", 0, nil)
	header := func(entries, dataSize int) []byte {
		h := []byte{0x8e, 0xad, 0xe8, 0x01, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(h[8:], uint32(entries))
		binary.BigEndian.PutUint32(h[12:], uint32(dataSize))
		return append(h, make([]byte, 16*entries+dataSize)...)
	}
	rpm := append([]byte{0xed, 0xab, 0xee, 0xdb}, make([]byte, 92)...)
	rpm = append(rpm, header(1, 5)...) // 16+16+5 = 37, padded to 40
	rpm = append(rpm, make([]byte, 3)...)
	rpm = append(rpm, header(2, 7)...)
	return append(rpm, zstdBytes(t, cpio.Bytes())...)
}

// testPackages returns a pinned Ubuntu 24.04 amd64 package carrying fakeBinary
// and a fetch function serving it.
func testPackages(t *testing.T, fakeBinary []byte) ([]hubPackage, func(context.Context, string) ([]byte, error)) {
	t.Helper()
	deb := testDeb(t, packageBinary, fakeBinary)
	pkg := hubPackage{ID: "ubuntu", VersionID: "24.04", Arch: "amd64", BinarySHA256: sha256Hex(fakeBinary)}
	pkg.Package.Format, pkg.Package.Version, pkg.Package.URL, pkg.Package.SHA256 = "deb", "test-1", "https://packages.example/hub.deb", sha256Hex(deb)
	return []hubPackage{pkg}, func(_ context.Context, url string) ([]byte, error) {
		if url != pkg.Package.URL {
			return nil, fmt.Errorf("unexpected url %q", url)
		}
		return deb, nil
	}
}

func openTestRoot(t *testing.T, osRelease string) (*os.Root, string) {
	t.Helper()
	dir := t.TempDir()
	if osRelease != "" {
		writeFixture(t, filepath.Join(dir, "usr/lib/os-release"), osRelease)
		if err := os.MkdirAll(filepath.Join(dir, "etc"), 0o755); err != nil {
			t.Fatal(err)
		}
		// Real images link /etc/os-release relatively to /usr/lib/os-release.
		if err := os.Symlink("../usr/lib/os-release", filepath.Join(dir, "etc/os-release")); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root, dir
}

const ubuntu2404 = "PRETTY_NAME=\"Ubuntu 24.04.4 LTS\"\nNAME=\"Ubuntu\"\nVERSION_ID=\"24.04\"\nID=ubuntu\nID_LIKE=debian\n"

func TestExtractPackageFile(t *testing.T) {
	t.Parallel()
	content := []byte("hub binary\x00with odd length")
	for format, pkg := range map[string][]byte{"deb": testDeb(t, packageBinary, content), "rpm": testRPM(t, packageBinary, content)} {
		t.Run(format, func(t *testing.T) {
			t.Parallel()
			got, err := extractPackageFile(format, pkg, packageBinary)
			if err != nil || !bytes.Equal(got, content) {
				t.Fatalf("extract = %q, %v", got, err)
			}
			if _, err := extractPackageFile(format, pkg, "usr/bin/other"); err == nil {
				t.Fatal("missing file was not reported")
			}
		})
	}
	for name, tt := range map[string]struct {
		format string
		pkg    []byte
	}{
		"unknown format": {"apk", nil},
		"not a deb":      {"deb", []byte("plain")},
		"not an rpm":     {"rpm", []byte("plain")},
		"truncated rpm":  {"rpm", testRPM(t, packageBinary, content)[:120]},
	} {
		if _, err := extractPackageFile(tt.format, tt.pkg, packageBinary); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestInstallBinary(t *testing.T) {
	t.Parallel()
	binary := []byte("fake hub binary")
	packages, fetch := testPackages(t, binary)
	downloads := 0
	counted := func(ctx context.Context, url string) ([]byte, error) { downloads++; return fetch(ctx, url) }
	root, dir := openTestRoot(t, ubuntu2404)
	for i := range 2 {
		version, err := installBinary(t.Context(), root, packages, "amd64", counted)
		if err != nil || version != "test-1" {
			t.Fatalf("installBinary #%d = %q, %v", i, version, err)
		}
	}
	if downloads != 1 {
		t.Fatalf("downloads = %d, want 1 (matching binary is not downloaded again)", downloads)
	}
	got, err := os.ReadFile(filepath.Join(dir, installedBinary))
	if err != nil || !bytes.Equal(got, binary) {
		t.Fatalf("installed binary = %q, %v", got, err)
	}
	info, err := os.Stat(filepath.Join(dir, installedBinary))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("installed binary mode = %v, %v", info, err)
	}
	// A stale or tampered binary is replaced with the pinned one.
	if err := os.WriteFile(filepath.Join(dir, installedBinary), []byte("stale"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := installBinary(t.Context(), root, packages, "amd64", counted); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, installedBinary)); !bytes.Equal(got, binary) {
		t.Fatal("stale binary not replaced")
	}
}

func TestInstallBinaryUnsupported(t *testing.T) {
	t.Parallel()
	packages, fetch := testPackages(t, []byte("fake"))
	tests := map[string]struct {
		osRelease string
		arch      string
	}{
		"other os":     {osRelease: "ID=ubuntu\nVERSION_ID=\"22.04\"\n", arch: "amd64"},
		"other distro": {osRelease: "ID=rhel\nVERSION_ID=\"9.4\"\n", arch: "amd64"},
		"other arch":   {osRelease: ubuntu2404, arch: "arm64"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root, dir := openTestRoot(t, tt.osRelease)
			_, err := installBinary(t.Context(), root, packages, tt.arch, fetch)
			if !errors.Is(err, ErrUnsupportedWorker) {
				t.Fatalf("error = %v, want ErrUnsupportedWorker", err)
			}
			if _, err := os.Stat(filepath.Join(dir, installedBinary)); !os.IsNotExist(err) {
				t.Fatal("installed a binary on an unsupported worker")
			}
		})
	}
}

func TestInstallBinaryRejectsBadPackage(t *testing.T) {
	t.Parallel()
	packages, _ := testPackages(t, []byte("expected"))
	tampered := testDeb(t, packageBinary, []byte("tampered"))
	pinnedTampered := append([]hubPackage(nil), packages...)
	pinnedTampered[0].Package.SHA256 = sha256Hex(tampered)
	for name, tt := range map[string]struct {
		packages []hubPackage
		fetch    func(context.Context, string) ([]byte, error)
	}{
		"download fails":   {packages, func(context.Context, string) ([]byte, error) { return nil, errors.New("offline") }},
		"package mismatch": {packages, func(context.Context, string) ([]byte, error) { return tampered, nil }},
		"binary mismatch":  {pinnedTampered, func(context.Context, string) ([]byte, error) { return tampered, nil }},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root, dir := openTestRoot(t, ubuntu2404)
			_, err := installBinary(t.Context(), root, tt.packages, "amd64", tt.fetch)
			if !errors.Is(err, ErrPackageDownload) {
				t.Fatalf("error = %v, want ErrPackageDownload", err)
			}
			if _, err := os.Stat(filepath.Join(dir, installedBinary)); !os.IsNotExist(err) {
				t.Fatal("installed an unverified binary")
			}
		})
	}
}

func TestPackagesCoverAgreedMatrix(t *testing.T) {
	t.Parallel()
	packages, err := loadPackages()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"ubuntu/24.04/amd64": false, "ubuntu/26.04/amd64": false, "azurelinux/3.0/amd64": false}
	for _, p := range packages {
		key := p.ID + "/" + p.VersionID + "/" + p.Arch
		if _, ok := want[key]; !ok || len(p.BinarySHA256) != 64 || len(p.Package.SHA256) != 64 ||
			!strings.HasPrefix(p.Package.URL, "https://packages.microsoft.com/") || (p.Package.Format != "deb" && p.Package.Format != "rpm") {
			t.Fatalf("unexpected or incomplete package %+v", p)
		}
		want[key] = true
	}
	for key, found := range want {
		if !found {
			t.Errorf("manifest missing %s", key)
		}
	}
}

func TestInstallBinaryBoundsDownload(t *testing.T) {
	t.Parallel()
	packages, fetch := testPackages(t, []byte("fake"))
	root, _ := openTestRoot(t, ubuntu2404)
	bounded := func(ctx context.Context, url string) ([]byte, error) {
		if _, ok := ctx.Deadline(); !ok {
			return nil, errors.New("download has no deadline")
		}
		return fetch(ctx, url)
	}
	if _, err := installBinary(t.Context(), root, packages, "amd64", bounded); err != nil {
		t.Fatal(err)
	}
}
