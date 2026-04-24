// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package utilio

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// IsDirEmpty reports whether dir is empty or does not exist.
func IsDirEmpty(dir string) (bool, error) {
	f, err := os.Open(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return true, nil
	case err != nil:
		return false, err
	}

	defer func() { _ = f.Close() }() //nolint:errcheck // best effort close

	_, err = f.Readdirnames(1)
	if errors.Is(err, io.EOF) {
		// no entry read
		return true, nil
	}

	return false, err
}

// CleanDir removes everything in a directory, but not the directory itself.
func CleanDir(path string) (retErr error) {
	_, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// nothing to do
		return nil
	case err != nil:
		return err
	default:
		// proceed to clean
	}

	d, err := os.Open(filepath.Clean(path))
	if err != nil {
		return err
	}

	defer func() {
		if cerr := d.Close(); cerr != nil && retErr == nil {
			retErr = fmt.Errorf("close directory: %w", cerr)
		}
	}()

	entries, err := d.Readdirnames(-1)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(path, entry)); err != nil {
			return err
		}
	}

	return nil
}
