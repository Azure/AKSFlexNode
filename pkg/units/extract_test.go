package units

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// buildTestRPM constructs a minimal RPM package in memory containing the
// given files. The RPM payload is a gzip-compressed CPIO "newc" archive.
func buildTestRPM(t *testing.T, files map[string]string) []byte {
	return buildTestRPMWithCompressor(t, files, "gzip")
}

// buildTestRPMWithCompressor constructs a minimal RPM package with the
// specified payload compression ("gzip" or "zstd").
func buildTestRPMWithCompressor(t *testing.T, files map[string]string, compressor string) []byte {
	return buildTestRPMWithSymlinks(t, files, nil, compressor)
}

// buildTestRPMWithSymlinks constructs a minimal RPM package with regular files
// and symlinks. The symlinks map keys are entry names and values are link targets.
func buildTestRPMWithSymlinks(t *testing.T, files, symlinks map[string]string, compressor string) []byte {
	t.Helper()
	var buf bytes.Buffer

	// RPM Lead (96 bytes).
	lead := make([]byte, 96)
	lead[0], lead[1], lead[2], lead[3] = 0xed, 0xab, 0xee, 0xdb // RPM magic
	buf.Write(lead)

	// Write a minimal RPM header (signature and main).
	for i := 0; i < 2; i++ {
		writeRPMHeader(t, &buf, i == 0)
	}

	// Build compressed CPIO payload.
	var cpioPayload bytes.Buffer
	var compressedWriter io.WriteCloser
	switch compressor {
	case "gzip":
		compressedWriter = gzip.NewWriter(&cpioPayload)
	case "zstd":
		w, err := zstd.NewWriter(&cpioPayload)
		if err != nil {
			t.Fatalf("creating zstd writer: %v", err)
		}
		compressedWriter = w
	default:
		t.Fatalf("unsupported compressor: %s", compressor)
	}

	for name, body := range files {
		writeCPIOEntry(t, compressedWriter, name, []byte(body), 0o100755)
	}
	for name, linkTarget := range symlinks {
		// Symlink mode: 0o120777 (type=symlink, perms=rwxrwxrwx)
		writeCPIOEntry(t, compressedWriter, name, []byte(linkTarget), 0o120777)
	}
	writeCPIOTrailer(t, compressedWriter)
	compressedWriter.Close()

	buf.Write(cpioPayload.Bytes())
	return buf.Bytes()
}

// writeRPMHeader writes a minimal RPM header structure with one dummy index entry.
func writeRPMHeader(t *testing.T, w io.Writer, alignTo8 bool) {
	t.Helper()
	// Magic
	w.Write([]byte{0x8e, 0xad, 0xe8, 0x01})
	// Reserved (4 bytes)
	w.Write(make([]byte, 4))
	// Index count: 1 entry
	binary.Write(w, binary.BigEndian, uint32(1))
	// Data size: 4 bytes of dummy data
	binary.Write(w, binary.BigEndian, uint32(4))
	// One index entry (16 bytes): tag=0, type=0, offset=0, count=0
	w.Write(make([]byte, 16))
	// Data (4 bytes)
	w.Write([]byte{0, 0, 0, 0})

	if alignTo8 {
		// Total header size from magic: 16 + 16 + 4 = 36
		// 36 % 8 = 4, pad = 4
		headerSize := 16 + 16 + 4
		pad := (8 - (headerSize % 8)) % 8
		if pad > 0 {
			w.Write(make([]byte, pad))
		}
	}
}

// writeCPIOEntry writes a single file entry in CPIO "newc" format.
func writeCPIOEntry(t *testing.T, w io.Writer, name string, data []byte, mode int) {
	t.Helper()
	nameWithNull := name + "\x00"
	nameLen := len(nameWithNull)

	hdr := fmt.Sprintf("070701"+
		"%08x"+ // inode
		"%08x"+ // mode
		"%08x"+ // uid
		"%08x"+ // gid
		"%08x"+ // nlink
		"%08x"+ // mtime
		"%08x"+ // filesize
		"%08x"+ // devmajor
		"%08x"+ // devminor
		"%08x"+ // rdevmajor
		"%08x"+ // rdevminor
		"%08x"+ // namesize
		"%08x", // checksum
		0,         // inode
		mode,      // mode
		0,         // uid
		0,         // gid
		1,         // nlink
		0,         // mtime
		len(data), // filesize
		0,         // devmajor
		0,         // devminor
		0,         // rdevmajor
		0,         // rdevminor
		nameLen,   // namesize
		0,         // checksum
	)
	w.Write([]byte(hdr))
	w.Write([]byte(nameWithNull))

	// Pad header+name to 4-byte boundary.
	headerAndName := 110 + nameLen
	namePad := (4 - (headerAndName % 4)) % 4
	if namePad > 0 {
		w.Write(make([]byte, namePad))
	}

	w.Write(data)

	// Pad data to 4-byte boundary.
	dataPad := (4 - (len(data) % 4)) % 4
	if dataPad > 0 {
		w.Write(make([]byte, dataPad))
	}
}

// writeCPIOTrailer writes the CPIO archive trailer entry.
func writeCPIOTrailer(t *testing.T, w io.Writer) {
	t.Helper()
	writeCPIOEntry(t, w, "TRAILER!!!", nil, 0)
}

func TestInstall_URLRPM(t *testing.T) {
	files := map[string]string{
		"./usr/bin/hello":     "hello-binary",
		"./usr/lib/libfoo.so": "libfoo-contents",
	}
	rpmData := buildTestRPM(t, files)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(rpmData)
	}))
	defer srv.Close()

	pkg, err := newOverlayPackage("testrpm", OverlayPackageDef{
		Version: "1.0.0",
		Source:  OverlayPackageSource{Type: overlayPackageSourceTypeURLRPM, URI: srv.URL + "/test.rpm"},
	})
	if err != nil {
		t.Fatalf("newOverlayPackage() error = %v", err)
	}

	base := filepath.Join(t.TempDir(), "pkg-out")
	if err := pkg.Install(context.Background(), base); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	// RPM paths have leading "./" stripped, so we expect usr/bin/hello etc.
	wantFiles := map[string]string{
		"usr/bin/hello":     "hello-binary",
		"usr/lib/libfoo.so": "libfoo-contents",
	}
	for name, wantBody := range wantFiles {
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

func TestInstall_URLRPM_PathTraversal(t *testing.T) {
	files := map[string]string{
		"../../etc/passwd": "pwned",
	}
	rpmData := buildTestRPM(t, files)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(rpmData)
	}))
	defer srv.Close()

	pkg, err := newOverlayPackage("evil-rpm", OverlayPackageDef{
		Version: "1.0.0",
		Source:  OverlayPackageSource{Type: overlayPackageSourceTypeURLRPM, URI: srv.URL + "/evil.rpm"},
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

func TestInstall_URLRPM_Zstd(t *testing.T) {
	files := map[string]string{
		"./usr/bin/hello":     "hello-binary-zstd",
		"./usr/lib/libfoo.so": "libfoo-zstd",
	}
	rpmData := buildTestRPMWithCompressor(t, files, "zstd")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(rpmData)
	}))
	defer srv.Close()

	pkg, err := newOverlayPackage("testrpm-zstd", OverlayPackageDef{
		Version: "1.0.0",
		Source:  OverlayPackageSource{Type: overlayPackageSourceTypeURLRPM, URI: srv.URL + "/test.rpm"},
	})
	if err != nil {
		t.Fatalf("newOverlayPackage() error = %v", err)
	}

	base := filepath.Join(t.TempDir(), "pkg-out")
	if err := pkg.Install(context.Background(), base); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	wantFiles := map[string]string{
		"usr/bin/hello":     "hello-binary-zstd",
		"usr/lib/libfoo.so": "libfoo-zstd",
	}
	for name, wantBody := range wantFiles {
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

func TestInstall_URLRPM_Symlink(t *testing.T) {
	// Build an RPM with a regular file and a symlink pointing to it.
	rpmData := buildTestRPMWithSymlinks(t, map[string]string{
		"./usr/lib/libfoo.so.1.0": "libfoo-real",
	}, map[string]string{
		"./usr/lib/libfoo.so": "libfoo.so.1.0", // relative symlink
	}, "gzip")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(rpmData)
	}))
	defer srv.Close()

	pkg, err := newOverlayPackage("testrpm-symlink", OverlayPackageDef{
		Version: "1.0.0",
		Source:  OverlayPackageSource{Type: overlayPackageSourceTypeURLRPM, URI: srv.URL + "/test.rpm"},
	})
	if err != nil {
		t.Fatalf("newOverlayPackage() error = %v", err)
	}

	base := filepath.Join(t.TempDir(), "pkg-out")
	if err := pkg.Install(context.Background(), base); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	// Verify the regular file.
	got, err := os.ReadFile(filepath.Join(base, "usr/lib/libfoo.so.1.0"))
	if err != nil {
		t.Fatalf("reading regular file: %v", err)
	}
	if string(got) != "libfoo-real" {
		t.Errorf("regular file content = %q, want %q", got, "libfoo-real")
	}

	// Verify the symlink exists and points to the right target.
	linkPath := filepath.Join(base, "usr/lib/libfoo.so")
	linkTarget, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatalf("readlink(%s): %v", linkPath, err)
	}
	if linkTarget != "libfoo.so.1.0" {
		t.Errorf("symlink target = %q, want %q", linkTarget, "libfoo.so.1.0")
	}

	// Reading through the symlink should give us the real file content.
	got, err = os.ReadFile(linkPath)
	if err != nil {
		t.Fatalf("reading through symlink: %v", err)
	}
	if string(got) != "libfoo-real" {
		t.Errorf("content through symlink = %q, want %q", got, "libfoo-real")
	}
}

func TestInstall_URLRPM_SymlinkPathTraversal(t *testing.T) {
	// A symlink that points outside the extraction base should be rejected.
	rpmData := buildTestRPMWithSymlinks(t, nil, map[string]string{
		"./usr/lib/evil": "../../../../etc/passwd",
	}, "gzip")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(rpmData)
	}))
	defer srv.Close()

	pkg, err := newOverlayPackage("evil-symlink-rpm", OverlayPackageDef{
		Version: "1.0.0",
		Source:  OverlayPackageSource{Type: overlayPackageSourceTypeURLRPM, URI: srv.URL + "/evil.rpm"},
	})
	if err != nil {
		t.Fatalf("newOverlayPackage() error = %v", err)
	}

	base := filepath.Join(t.TempDir(), "pkg-out")
	err = pkg.Install(context.Background(), base)
	if err == nil {
		t.Fatal("Install() should have failed for symlink path traversal")
	}
}

// buildTestDeb constructs a minimal Debian package in memory containing the
// given files. The package uses the standard ar format with data.tar.gz.
func buildTestDeb(t *testing.T, files map[string]string) []byte {
	return buildTestDebWithSymlinks(t, files, nil)
}

// buildTestDebWithSymlinks constructs a minimal Debian package with regular
// files and symlinks. The symlinks map keys are entry names and values are
// link targets.
func buildTestDebWithSymlinks(t *testing.T, files, symlinks map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer

	// ar global header.
	buf.WriteString("!<arch>\n")

	// debian-binary member.
	debBinaryContent := []byte("2.0\n")
	writeArMember(t, &buf, "debian-binary", debBinaryContent)

	// control.tar.gz member (minimal, just an empty tar).
	var controlTar bytes.Buffer
	gw := gzip.NewWriter(&controlTar)
	tw := tar.NewWriter(gw)
	tw.Close()
	gw.Close()
	writeArMember(t, &buf, "control.tar.gz", controlTar.Bytes())

	// data.tar.gz member with actual file contents.
	var dataTar bytes.Buffer
	gw2 := gzip.NewWriter(&dataTar)
	tw2 := tar.NewWriter(gw2)
	for name, body := range files {
		hdr := &tar.Header{
			Name: "./" + name,
			Mode: 0755,
			Size: int64(len(body)),
		}
		if err := tw2.WriteHeader(hdr); err != nil {
			t.Fatalf("writing tar header for %s: %v", name, err)
		}
		if _, err := tw2.Write([]byte(body)); err != nil {
			t.Fatalf("writing tar body for %s: %v", name, err)
		}
	}
	for name, linkTarget := range symlinks {
		hdr := &tar.Header{
			Typeflag: tar.TypeSymlink,
			Name:     "./" + name,
			Linkname: linkTarget,
			Mode:     0777,
		}
		if err := tw2.WriteHeader(hdr); err != nil {
			t.Fatalf("writing tar symlink header for %s: %v", name, err)
		}
	}
	tw2.Close()
	gw2.Close()
	writeArMember(t, &buf, "data.tar.gz", dataTar.Bytes())

	return buf.Bytes()
}

// writeArMember writes a single ar member with the given name and content.
func writeArMember(t *testing.T, w *bytes.Buffer, name string, content []byte) {
	t.Helper()
	// ar member header: 60 bytes
	// name(16) + mtime(12) + uid(6) + gid(6) + mode(8) + size(10) + magic(2)
	header := fmt.Sprintf("%-16s%-12s%-6s%-6s%-8s%-10d`\n",
		name+"/",
		"0",
		"0",
		"0",
		"100644",
		len(content),
	)
	w.WriteString(header)
	w.Write(content)

	// Pad to 2-byte boundary.
	if len(content)%2 != 0 {
		w.WriteByte('\n')
	}
}

func TestInstall_URLDeb(t *testing.T) {
	files := map[string]string{
		"usr/bin/hello":       "hello-binary",
		"usr/lib/libbar.so.1": "libbar-contents",
	}
	debData := buildTestDeb(t, files)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(debData)
	}))
	defer srv.Close()

	pkg, err := newOverlayPackage("testdeb", OverlayPackageDef{
		Version: "1.0.0",
		Source:  OverlayPackageSource{Type: overlayPackageSourceTypeURLDeb, URI: srv.URL + "/test.deb"},
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

// TestExtractRPM_RealAzureLinux validates the RPM extractor against a real
// Azure Linux zstd-compressed RPM. The test is skipped if the RPM file is not
// present at /tmp/runc-test.rpm. To populate it:
//
//	curl -Lo /tmp/runc-test.rpm https://packages.microsoft.com/azurelinux/3.0/prod/base/x86_64/Packages/r/runc-1.3.3-1.azl3.x86_64.rpm
func TestExtractRPM_RealAzureLinux(t *testing.T) {
	const rpmPath = "/tmp/runc-test.rpm"
	if _, err := os.Stat(rpmPath); os.IsNotExist(err) {
		t.Skipf("skipping: real RPM not found at %s", rpmPath)
	}

	outDir := filepath.Join(t.TempDir(), "runc-extracted")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("creating output dir: %v", err)
	}

	if err := extractRPM(rpmPath, outDir); err != nil {
		t.Fatalf("extractRPM() error = %v", err)
	}

	// The runc RPM should contain at least the runc binary.
	runcBin := filepath.Join(outDir, "usr/bin/runc")
	info, err := os.Stat(runcBin)
	if err != nil {
		t.Fatalf("expected runc binary at usr/bin/runc: %v", err)
	}
	if info.Size() < 1024 {
		t.Errorf("runc binary unexpectedly small: %d bytes", info.Size())
	}
	t.Logf("runc binary: %d bytes, mode=%s", info.Size(), info.Mode())

	// Walk the extracted tree and log all files for manual inspection.
	var fileCount, dirCount int
	err = filepath.Walk(outDir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(outDir, path)
		if fi.IsDir() {
			dirCount++
			t.Logf("  dir:  %s/", rel)
		} else {
			fileCount++
			t.Logf("  file: %s  (%d bytes, %s)", rel, fi.Size(), fi.Mode())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking extracted tree: %v", err)
	}
	t.Logf("Total: %d files, %d directories", fileCount, dirCount)

	if fileCount == 0 {
		t.Error("no files extracted from RPM")
	}
}

func TestInstall_URLDeb_PathTraversal(t *testing.T) {
	files := map[string]string{
		"../../etc/passwd": "pwned",
	}
	debData := buildTestDeb(t, files)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(debData)
	}))
	defer srv.Close()

	pkg, err := newOverlayPackage("evil-deb", OverlayPackageDef{
		Version: "1.0.0",
		Source:  OverlayPackageSource{Type: overlayPackageSourceTypeURLDeb, URI: srv.URL + "/evil.deb"},
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

func TestInstall_URLDeb_Symlink(t *testing.T) {
	debData := buildTestDebWithSymlinks(t, map[string]string{
		"usr/lib/libbar.so.1.0": "libbar-real",
	}, map[string]string{
		"usr/lib/libbar.so": "libbar.so.1.0",
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(debData)
	}))
	defer srv.Close()

	pkg, err := newOverlayPackage("testdeb-symlink", OverlayPackageDef{
		Version: "1.0.0",
		Source:  OverlayPackageSource{Type: overlayPackageSourceTypeURLDeb, URI: srv.URL + "/test.deb"},
	})
	if err != nil {
		t.Fatalf("newOverlayPackage() error = %v", err)
	}

	base := filepath.Join(t.TempDir(), "pkg-out")
	if err := pkg.Install(context.Background(), base); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	// Verify the regular file.
	got, err := os.ReadFile(filepath.Join(base, "usr/lib/libbar.so.1.0"))
	if err != nil {
		t.Fatalf("reading regular file: %v", err)
	}
	if string(got) != "libbar-real" {
		t.Errorf("regular file content = %q, want %q", got, "libbar-real")
	}

	// Verify the symlink.
	linkPath := filepath.Join(base, "usr/lib/libbar.so")
	linkTarget, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatalf("readlink(%s): %v", linkPath, err)
	}
	if linkTarget != "libbar.so.1.0" {
		t.Errorf("symlink target = %q, want %q", linkTarget, "libbar.so.1.0")
	}

	// Reading through the symlink should give us the real file content.
	got, err = os.ReadFile(linkPath)
	if err != nil {
		t.Fatalf("reading through symlink: %v", err)
	}
	if string(got) != "libbar-real" {
		t.Errorf("content through symlink = %q, want %q", got, "libbar-real")
	}
}

func TestInstall_URLDeb_SymlinkPathTraversal(t *testing.T) {
	debData := buildTestDebWithSymlinks(t, nil, map[string]string{
		"usr/lib/evil": "../../../../etc/passwd",
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(debData)
	}))
	defer srv.Close()

	pkg, err := newOverlayPackage("evil-symlink-deb", OverlayPackageDef{
		Version: "1.0.0",
		Source:  OverlayPackageSource{Type: overlayPackageSourceTypeURLDeb, URI: srv.URL + "/evil.deb"},
	})
	if err != nil {
		t.Fatalf("newOverlayPackage() error = %v", err)
	}

	base := filepath.Join(t.TempDir(), "pkg-out")
	err = pkg.Install(context.Background(), base)
	if err == nil {
		t.Fatal("Install() should have failed for symlink path traversal")
	}
}
