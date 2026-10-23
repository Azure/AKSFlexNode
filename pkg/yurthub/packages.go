package yurthub

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
)

// installedBinary is agent-owned, so it never replaces a yurthub that a
// customer's worker image may already ship under /usr/bin.
const installedBinary = "usr/local/bin/aks-flex-yurthub"

const (
	// packageBinary is where the Dalec packages install Hub.
	packageBinary   = "usr/bin/yurthub"
	maxPackageBytes = 128 << 20
	maxBinaryBytes  = 256 << 20
	// downloadTimeout bounds the download because it runs on the daemon's
	// serialized queue.
	downloadTimeout = 3 * time.Minute
)

// ErrUnsupportedWorker means no pinned Hub package matches the worker image's OS
// or architecture. Callers log it and keep the node on the direct API path; it
// is not retried as a failure.
var ErrUnsupportedWorker = errors.New("hub is not available for this worker")

// ErrPackageDownload means no valid pinned Hub package could be downloaded, for
// example because packages.microsoft.com is unreachable or a proxy altered it.
var ErrPackageDownload = errors.New("could not download a valid hub package")

//go:embed packages.json
var packagesJSON []byte

type hubPackage struct {
	ID           string `json:"id"`
	VersionID    string `json:"versionId"`
	Arch         string `json:"arch"`
	BinarySHA256 string `json:"binarySHA256"`
	Package      struct {
		Format  string `json:"format"`
		Version string `json:"version"`
		URL     string `json:"url"`
		SHA256  string `json:"sha256"`
	} `json:"package"`
}

func loadPackages() ([]hubPackage, error) {
	var manifest struct {
		Variants []hubPackage `json:"variants"`
	}
	if err := json.Unmarshal(packagesJSON, &manifest); err != nil {
		return nil, fmt.Errorf("decode hub package manifest: %w", err)
	}
	return manifest.Variants, nil
}

// workerOS reads ID and VERSION_ID from the worker image's os-release.
func workerOS(root *os.Root) (id, versionID string, err error) {
	var data []byte
	for _, name := range []string{"etc/os-release", "usr/lib/os-release"} {
		if data, err = root.ReadFile(name); err == nil {
			break
		}
	}
	if err != nil {
		return "", "", fmt.Errorf("read worker os-release: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		value = strings.Trim(value, `"'`)
		switch key {
		case "ID":
			id = value
		case "VERSION_ID":
			versionID = value
		}
	}
	return id, versionID, nil
}

// installBinary installs the pinned Hub binary for the worker image and returns
// its package version. It downloads only when the installed binary differs from
// the pin, and verifies both the package and binary checksums before writing.
func installBinary(ctx context.Context, root *os.Root, packages []hubPackage, arch string, fetch func(context.Context, string) ([]byte, error)) (string, error) {
	id, versionID, err := workerOS(root)
	if err != nil {
		return "", err
	}
	var pkg *hubPackage
	for i := range packages {
		if packages[i].ID == id && packages[i].VersionID == versionID && packages[i].Arch == arch {
			pkg = &packages[i]
			break
		}
	}
	if pkg == nil {
		return "", fmt.Errorf("%w: no hub package for worker image %s %s on %s", ErrUnsupportedWorker, id, versionID, arch)
	}
	if installedSHA256(root) == pkg.BinarySHA256 {
		return pkg.Package.Version, nil
	}
	fetchCtx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	data, err := fetch(fetchCtx, pkg.Package.URL)
	if err != nil {
		return "", fmt.Errorf("%w from %s: %w", ErrPackageDownload, pkg.Package.URL, err)
	}
	if sha256Hex(data) != pkg.Package.SHA256 {
		return "", fmt.Errorf("%w: %s does not match its pinned checksum", ErrPackageDownload, pkg.Package.URL)
	}
	binary, err := extractPackageFile(pkg.Package.Format, data, packageBinary)
	if err != nil {
		return "", fmt.Errorf("%w: extract hub binary: %w", ErrPackageDownload, err)
	}
	if sha256Hex(binary) != pkg.BinarySHA256 {
		return "", fmt.Errorf("%w: hub binary does not match its pinned checksum", ErrPackageDownload)
	}
	if err := utilio.WriteFileInRoot(root, installedBinary, binary, 0o755); err != nil {
		return "", fmt.Errorf("install hub binary: %w", err)
	}
	return pkg.Package.Version, nil
}

func downloadPackage(ctx context.Context, url string) ([]byte, error) {
	return utilio.DownloadBytes(ctx, url, maxPackageBytes)
}

// extractPackageFile returns one regular file from a zstd-compressed .deb or .rpm,
// the formats Dalec publishes. Inputs are already checksum-verified.
func extractPackageFile(format string, pkg []byte, name string) ([]byte, error) {
	var payload []byte
	var err error
	switch format {
	case "deb":
		payload, err = debData(pkg)
	case "rpm":
		payload, err = rpmPayload(pkg)
	default:
		return nil, fmt.Errorf("unsupported package format %q", format)
	}
	if err != nil {
		return nil, err
	}
	zr, err := zstd.NewReader(bytes.NewReader(payload), zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	if format == "deb" {
		return tarFile(zr, name)
	}
	return cpioFile(zr, name)
}

// debData returns data.tar.zst from a .deb (an ar archive).
func debData(pkg []byte) ([]byte, error) {
	const magic = "!<arch>\n"
	if !bytes.HasPrefix(pkg, []byte(magic)) {
		return nil, fmt.Errorf("not a deb package")
	}
	for off := len(magic); off+60 <= len(pkg); {
		header := pkg[off : off+60]
		size, err := strconv.Atoi(strings.TrimSpace(string(header[48:58])))
		if err != nil || size < 0 || off+60+size > len(pkg) {
			return nil, fmt.Errorf("invalid deb member")
		}
		body := pkg[off+60 : off+60+size]
		if strings.TrimRight(strings.TrimSpace(string(header[:16])), "/") == "data.tar.zst" {
			return body, nil
		}
		off += 60 + size + size%2
	}
	return nil, fmt.Errorf("deb has no data.tar.zst")
}

// rpmPayload skips the rpm lead and both headers and returns the payload.
func rpmPayload(pkg []byte) ([]byte, error) {
	const leadSize = 96
	if len(pkg) < leadSize || !bytes.HasPrefix(pkg, []byte{0xed, 0xab, 0xee, 0xdb}) {
		return nil, fmt.Errorf("not an rpm package")
	}
	off := leadSize
	for i := range 2 {
		if off+16 > len(pkg) || !bytes.HasPrefix(pkg[off:], []byte{0x8e, 0xad, 0xe8, 0x01}) {
			return nil, fmt.Errorf("invalid rpm header")
		}
		entries := int(binary.BigEndian.Uint32(pkg[off+8:]))
		dataSize := int(binary.BigEndian.Uint32(pkg[off+12:]))
		off += 16 + 16*entries + dataSize
		if i == 0 {
			off += (8 - off%8) % 8 // the signature header is padded to 8 bytes
		}
		if off > len(pkg) {
			return nil, fmt.Errorf("invalid rpm header")
		}
	}
	return pkg[off:], nil
}

func tarFile(r io.Reader, name string) ([]byte, error) {
	tr := tar.NewReader(r)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("package has no %s", name)
		}
		if err != nil {
			return nil, err
		}
		if path.Clean(strings.TrimPrefix(header.Name, "./")) == name && header.Typeflag == tar.TypeReg {
			return readLimited(tr, header.Size)
		}
	}
}

// cpioFile reads the "newc" cpio format used by rpm payloads.
func cpioFile(r io.Reader, name string) ([]byte, error) {
	header := make([]byte, 110)
	for {
		if _, err := io.ReadFull(r, header); err != nil {
			return nil, fmt.Errorf("read cpio header: %w", err)
		}
		if string(header[:6]) != "070701" {
			return nil, fmt.Errorf("unsupported cpio format")
		}
		field := func(i int) (int64, error) {
			return strconv.ParseInt(string(header[6+8*i:14+8*i]), 16, 64)
		}
		mode, err1 := field(1)
		size, err2 := field(6)
		nameSize, err3 := field(11)
		if err := errors.Join(err1, err2, err3); err != nil || nameSize < 1 || nameSize > 4096 || size < 0 {
			return nil, fmt.Errorf("invalid cpio header")
		}
		// Header plus name, and file data, are each padded to 4 bytes.
		nameBuf := make([]byte, nameSize+(4-(110+nameSize)%4)%4)
		if _, err := io.ReadFull(r, nameBuf); err != nil {
			return nil, fmt.Errorf("read cpio name: %w", err)
		}
		entry := strings.TrimRight(string(nameBuf[:nameSize]), "\x00")
		if entry == "TRAILER!!!" {
			return nil, fmt.Errorf("package has no %s", name)
		}
		if path.Clean(strings.TrimPrefix(entry, "./")) == name && mode&0o170000 == 0o100000 {
			return readLimited(r, size)
		}
		if _, err := io.CopyN(io.Discard, r, size+(4-size%4)%4); err != nil {
			return nil, fmt.Errorf("skip cpio entry: %w", err)
		}
	}
}

func readLimited(r io.Reader, size int64) ([]byte, error) {
	if size > maxBinaryBytes {
		return nil, fmt.Errorf("packaged file exceeds %d bytes", maxBinaryBytes)
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, err
	}
	return data, nil
}

// installedSHA256 streams the installed binary so routine checks don't hold a
// second copy in memory. Empty means absent or unreadable.
func installedSHA256(root *os.Root) string {
	f, err := root.Open(installedBinary)
	if err != nil {
		return ""
	}
	defer f.Close() //nolint:errcheck // read-only
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, maxBinaryBytes+1)); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
