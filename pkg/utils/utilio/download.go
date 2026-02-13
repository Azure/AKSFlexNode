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
		resp.Body.Close()
		return nil, fmt.Errorf("download %q failed with status code %d", url, resp.StatusCode)
	}

	return resp.Body, nil
}

type TarFile struct {
	Header *tar.Header
	Body   io.Reader
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

			if !yield(&TarFile{Header: header, Body: tarReader}, nil) {
				return
			}
		}
	}
}

func DownloadToLocalFile(ctx context.Context, url string, filename string, perm os.FileMode) error {
	body, err := downloadFromRemote(ctx, url)
	if err != nil {
		return err
	}
	defer body.Close()

	content, err := ReadAll1GiB(body) // FIXME: allow configuring max file size
	if err != nil {
		return fmt.Errorf("failed to read downloaded content: %w", err)
	}

	return WriteFile(filename, content, perm)
}
