// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package utilio

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
		_ = resp.Body.Close() //nolint:errcheck // body close
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
		defer body.Close() //nolint:errcheck // body close

		gzipStream, err := gzip.NewReader(body)
		if err != nil {
			yield(nil, err)
			return
		}
		defer gzipStream.Close() //nolint:errcheck // gzip reader close

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

// cleanedTarEntryName validates and cleans a tar entry name to prevent path traversal attacks.
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
func DownloadToLocalFile(ctx context.Context, url, filename string, perm os.FileMode) error {
	body, err := downloadFromRemote(ctx, url)
	if err != nil {
		return err
	}
	defer body.Close() //nolint:errcheck // body close

	return InstallFile(filename, body, perm)
}

// DownloadWithSHA256Verification downloads content from the given URL and verifies it against the SHA256
// checksum fetched from checksumURL. The checksum file is expected to contain a hex-encoded SHA256 hash
// (optionally followed by whitespace and a filename, which is ignored).
//
// NOTE: we assume the filename is trusted and cleaned without path traversal characters.
func DownloadWithSHA256Verification(ctx context.Context, url, checksumURL, filename string, perm os.FileMode) error {
	expectedHash, err := fetchSHA256(ctx, checksumURL)
	if err != nil {
		return fmt.Errorf("fetch checksum from %q: %w", checksumURL, err)
	}

	body, err := downloadFromRemote(ctx, url)
	if err != nil {
		return err
	}
	defer body.Close() //nolint:errcheck // body close

	hasher := sha256.New()
	teeReader := io.TeeReader(body, hasher)

	if err := InstallFile(filename, teeReader, perm); err != nil {
		return err
	}

	actualHash := hex.EncodeToString(hasher.Sum(nil))
	if actualHash != expectedHash {
		// Remove the file that failed verification.
		_ = os.Remove(filename) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("SHA256 mismatch for %q: expected %s, got %s", url, expectedHash, actualHash)
	}

	return nil
}

// fetchSHA256 downloads and parses a SHA256 checksum file. The file is expected to contain a hex-encoded
// hash, optionally followed by whitespace and a filename (standard sha256sum output format).
func fetchSHA256(ctx context.Context, checksumURL string) (string, error) {
	body, err := downloadFromRemote(ctx, checksumURL)
	if err != nil {
		return "", err
	}
	defer body.Close() //nolint:errcheck // body close

	// Checksum files are small; limit to 1 KiB to prevent abuse.
	raw, err := io.ReadAll(io.LimitReader(body, 1024))
	if err != nil {
		return "", fmt.Errorf("read checksum body: %w", err)
	}

	// Parse: the file may be just the hex hash, or "hash  filename\n" (sha256sum format).
	hashStr := strings.TrimSpace(string(raw))
	if fields := strings.Fields(hashStr); len(fields) >= 1 {
		hashStr = fields[0]
	}

	if len(hashStr) != sha256.Size*2 {
		return "", fmt.Errorf("invalid SHA256 hash length %d in checksum file", len(hashStr))
	}

	// Validate that the string is valid hex.
	if _, err := hex.DecodeString(hashStr); err != nil {
		return "", fmt.Errorf("invalid hex in checksum file: %w", err)
	}

	return hashStr, nil
}
