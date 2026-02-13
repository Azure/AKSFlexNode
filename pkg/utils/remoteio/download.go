package remoteio

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"time"
)

var remoteHTTPClient = &http.Client{
	Timeout: 10 * time.Minute, // FIXME: proper configuration
}

type TarFile struct {
	Header *tar.Header
	Body   io.Reader
}

// DecompressTarGzFromRemote returns an iterator that yields the files contained in a .tar.gz file located at the given URL.
func DecompressTarGzFromRemote(ctx context.Context, url string) iter.Seq2[*TarFile, error] {
	return func(yield func(*TarFile, error) bool) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
		if err != nil {
			yield(nil, err)
			return
		}

		resp, err := remoteHTTPClient.Do(req)
		if err != nil {
			yield(nil, err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			err := fmt.Errorf("download %q failed with status code %d", url, resp.StatusCode)
			yield(nil, err)
			return
		}

		gzipStream, err := gzip.NewReader(resp.Body)
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
