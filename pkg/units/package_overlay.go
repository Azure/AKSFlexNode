package units

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	// OverlayPackageSourceTypeURL indicates that the package source is a direct URL to a file.
	overlayPackageSourceTypeURL = "url"
	// OverlayPackageSourceTypeURLTar indicates that the package source is a URL to a tarball that needs to be downloaded and extracted.
	overlayPackageSourceTypeURLTar = "url+tar"
	// OverlayPackageSourceTypeURLZip indicates that the package source is a URL to a zip file that needs to be downloaded and extracted.
	overlayPackageSourceTypeURLZip = "url+zip"
	// OverlayPackageSourceTypeFile indicates that the package source is a local file.
	overlayPackageSourceTypeFile = "file"
)

var validOverlayPackageSourceTypes = map[string]struct{}{
	overlayPackageSourceTypeURL:    {},
	overlayPackageSourceTypeURLTar: {},
	overlayPackageSourceTypeURLZip: {},
	overlayPackageSourceTypeFile:   {},
}

type overlayPackage struct {
	name string
	def  OverlayPackageDef
}

func newOverlayPackage(name string, def OverlayPackageDef) (*overlayPackage, error) {
	if def.Version == "" {
		return nil, fmt.Errorf("version is required for package %q", name)
	}
	if _, ok := validOverlayPackageSourceTypes[def.Source.Type]; !ok {
		return nil, fmt.Errorf("invalid source type %q for package %q", def.Source.Type, name)
	}
	if def.Source.URI == "" {
		return nil, fmt.Errorf("source URI is required for package %q", name)
	}
	for _, etcFile := range def.ETCFiles {
		if etcFile.Source == "" {
			return nil, fmt.Errorf("etc file source is required for package %q", name)
		}
		if etcFile.Target == "" {
			return nil, fmt.Errorf("etc file target is required for package %q", name)
		}
	}

	return &overlayPackage{name: name, def: def}, nil
}

var _ Package = (*overlayPackage)(nil)

func (o *overlayPackage) Name() string {
	return o.name
}

func (o *overlayPackage) Sources() []string {
	return []string{
		fmt.Sprintf("%s|%s", o.def.Source.Type, o.def.Source.URI),
	}
}

func (o *overlayPackage) Version() string {
	return o.def.Version
}

func (o *overlayPackage) EtcFiles() []PackageEtcFile {
	return slices.Clone(o.def.ETCFiles)
}

func (o *overlayPackage) Install(ctx context.Context, base string) error {
	if err := os.MkdirAll(base, dirPermissions); err != nil {
		return fmt.Errorf("creating base directory %s: %w", base, err)
	}

	switch o.def.Source.Type {
	case overlayPackageSourceTypeURL:
		return o.installFromURL(ctx, base)
	case overlayPackageSourceTypeURLTar, overlayPackageSourceTypeURLZip:
		return o.installFromURLArchive(ctx, base)
	case overlayPackageSourceTypeFile:
		return o.installFromFile(base)
	default:
		return fmt.Errorf("unsupported source type %q for package %q", o.def.Source.Type, o.name)
	}
}

// overlayPackageClient is the shared HTTP client for downloading package sources.
// TODO: retry / UA / backoff etc
var overlayPackageClient = &http.Client{
	Timeout: 10 * time.Minute,
}

// download fetches the source URI and writes the response body to dst.
func (o *overlayPackage) download(ctx context.Context, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.def.Source.URI, nil)
	if err != nil {
		return fmt.Errorf("creating request for %s: %w", o.def.Source.URI, err)
	}

	resp, err := overlayPackageClient.Do(req)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", o.def.Source.URI, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading %s: status %d", o.def.Source.URI, resp.StatusCode)
	}

	f, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("creating file %s: %w", dst, err)
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return fmt.Errorf("writing file %s: %w", dst, err)
	}

	return nil
}

// installFromURL downloads a single file from the source URI into base,
// preserving the filename from the URL path.
func (o *overlayPackage) installFromURL(ctx context.Context, base string) error {
	dst := filepath.Join(base, filepath.Base(o.def.Source.URI))
	return o.download(ctx, dst)
}

// installFromURLArchive downloads an archive (tar.gz or zip) from the source
// URI and extracts its contents into base.
func (o *overlayPackage) installFromURLArchive(ctx context.Context, base string) error {
	tmpFile, err := os.CreateTemp("", "overlay-pkg-*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	if err := o.download(ctx, tmpFile.Name()); err != nil {
		return err
	}

	switch o.def.Source.Type {
	case overlayPackageSourceTypeURLTar:
		return extractTar(tmpFile.Name(), base)
	case overlayPackageSourceTypeURLZip:
		return extractZip(tmpFile.Name(), base)
	default:
		return fmt.Errorf("unsupported archive type %q", o.def.Source.Type)
	}
}

// installFromFile copies a local file or directory into base.
// If the source is a file, it is copied preserving the filename.
// If the source is a directory, its contents are recursively copied into base.
func (o *overlayPackage) installFromFile(base string) error {
	info, err := os.Stat(o.def.Source.URI)
	if err != nil {
		return fmt.Errorf("stat source %s: %w", o.def.Source.URI, err)
	}

	if !info.IsDir() {
		return copyFile(o.def.Source.URI, filepath.Join(base, filepath.Base(o.def.Source.URI)))
	}

	return filepath.WalkDir(o.def.Source.URI, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(o.def.Source.URI, path)
		if err != nil {
			return fmt.Errorf("computing relative path for %s: %w", path, err)
		}

		target := filepath.Join(base, rel)

		if d.IsDir() {
			return os.MkdirAll(target, dirPermissions)
		}

		return copyFile(path, target)
	})
}

// copyFile copies a single file from src to dst, preserving the source file mode.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening %s: %w", src, err)
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return fmt.Errorf("creating %s: %w", dst, err)
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("writing %s: %w", dst, err)
	}

	return nil
}

// safePath validates that name does not escape base via path traversal and
// returns the joined, cleaned target path.
func safePath(base, name string) (string, error) {
	target := filepath.Join(base, name)
	cleanBase := filepath.Clean(base)
	cleanTarget := filepath.Clean(target)
	if cleanTarget != cleanBase && !strings.HasPrefix(cleanTarget, cleanBase+string(os.PathSeparator)) {
		return "", fmt.Errorf("archive entry %q escapes base directory", name)
	}
	return target, nil
}

// extractEntry writes a single archive entry (directory or regular file) to
// the target path under base.
func extractEntry(target string, mode os.FileMode, isDir bool, r io.Reader) error {
	if isDir {
		return os.MkdirAll(target, dirPermissions)
	}

	if err := os.MkdirAll(filepath.Dir(target), dirPermissions); err != nil {
		return fmt.Errorf("creating parent directory for %s: %w", target, err)
	}

	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("creating file %s: %w", target, err)
	}
	defer f.Close()

	if _, err := io.Copy(f, r); err != nil {
		return fmt.Errorf("writing file %s: %w", target, err)
	}

	return nil
}

// extractTar extracts a gzipped tarball at src into base.
func extractTar(src, base string) error {
	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening tar file %s: %w", src, err)
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("reading gzip stream from %s: %w", src, err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading tar entry: %w", err)
		}

		target, err := safePath(base, hdr.Name)
		if err != nil {
			return err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := extractEntry(target, 0, true, nil); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := extractEntry(target, os.FileMode(hdr.Mode), false, tr); err != nil {
				return err
			}
		}
	}

	return nil
}

// extractZip extracts a zip archive at src into base.
func extractZip(src, base string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return fmt.Errorf("opening zip file %s: %w", src, err)
	}
	defer zr.Close()

	for _, zf := range zr.File {
		target, err := safePath(base, zf.Name)
		if err != nil {
			return err
		}

		if zf.FileInfo().IsDir() {
			if err := extractEntry(target, 0, true, nil); err != nil {
				return err
			}
			continue
		}

		rc, err := zf.Open()
		if err != nil {
			return fmt.Errorf("opening zip entry %s: %w", zf.Name, err)
		}

		if err := extractEntry(target, zf.Mode(), false, rc); err != nil {
			rc.Close()
			return err
		}
		rc.Close()
	}

	return nil
}
