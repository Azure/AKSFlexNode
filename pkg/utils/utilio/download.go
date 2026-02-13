package utilio

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var remoteHTTPClient = &http.Client{
	Timeout: 10 * time.Minute, // FIXME: proper configuration
}

// FIXME: harden to mitigate SSRF
func downloadFromRemote(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}

	resp, err := remoteHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to perform HTTP request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("download %q failed with status code %d", url, resp.StatusCode)
	}

	return resp.Body, nil
}

type TarFile struct {
	Name string
	Body io.Reader
}

// DecompressTarGzFromRemote returns an iterator that yields the files contained in a .tar.gz file located at the given URL.
func DecompressTarGzFromRemote(ctx context.Context, url string) iter.Seq2[*TarFile, error] {
	return func(yield func(*TarFile, error) bool) {
		body, err := downloadFromRemote(ctx, url)
		if err != nil {
			yield(nil, err)
			return
		}
		defer body.Close()

		gzipStream, err := gzip.NewReader(body)
		if err != nil {
			yield(nil, err)
			return
		}
		defer gzipStream.Close()

		tarReader := tar.NewReader(gzipStream)

		for {
			header, err := tarReader.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				yield(nil, err)
				return
			}

			if header.Typeflag != tar.TypeReg {
				continue
			}

			cleanedName, err := cleanedTarEntryName(header.Name)
			if err != nil {
				yield(nil, fmt.Errorf("invalid tar entry %q: %w", header.Name, err))
				return
			}

			if !yield(&TarFile{Name: cleanedName, Body: tarReader}, nil) {
				return
			}
		}
	}
}

// to avoid common path traversal mistakes
func cleanedTarEntryName(filename string) (string, error) {
	if filename == "" {
		return "", fmt.Errorf("invalid tar entry name: %q", filename)
	}
	// Tar paths should be forward-slash. Reject backslashes to avoid odd edge cases.
	if strings.Contains(filename, `\`) || strings.ContainsRune(filename, '\x00') {
		return "", fmt.Errorf("invalid tar entry name: %q", filename)
	}

	cleaned := filepath.Clean(filepath.FromSlash(filename))
	if filepath.IsAbs(cleaned) ||
		cleaned == "." || cleaned == ".." ||
		strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid tar entry name: %q", filename)
	}
	return cleaned, nil
}

// DownloadToLocalFile downloads content from giving URL to local file and sets the specified permissions.
// It limits the size of the content to 1 GiB and returns an error if the limit is exceeded.
// It ensures that the target directory exists and handles the file writing atomically.
//
// NOTE: we assume the filename is trusted and cleaned without path traversal characters.
func DownloadToLocalFile(ctx context.Context, url string, filename string, perm os.FileMode) error {
	body, err := downloadFromRemote(ctx, url)
	if err != nil {
		return err
	}
	defer body.Close()

	return InstallFile(filename, body, perm)
}
