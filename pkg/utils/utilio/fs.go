package utilio

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

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

	d, err := os.Open(filepath.Clean(path)) // #nosec G304 -- path is caller-controlled
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
