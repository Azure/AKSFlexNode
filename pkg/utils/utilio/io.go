package utilio

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/google/renameio/v2"
)

var ErrFileTooLarge = errors.New("file exceeds maximum allowed size")

// InstallFile writes the content from the provided reader to a local file with specified permissions.
// It limits the size of the content to 1 GiB and returns an error if the limit is exceeded.
// It ensures that the target directory exists and handles the file writing atomically.
//
// NOTE: we assume the filename is trusted and cleaned without path traversal characters.
func InstallFile(filename string, r io.Reader, perm os.FileMode) error {
	const maxFileSize = 1 * 1024 * 1024 * 1024 // 1 GiB
	return InstallFileWithLimitedSize(filename, r, perm, maxFileSize)
}

// InstallFileWithLimitedSize streams content to local file with limited size and specified permissions.
// It ensures that the target directory exists and handles the file writing atomically.
//
// NOTE: we assume the filename is trusted and cleaned without path traversal characters.
func InstallFileWithLimitedSize(filename string, r io.Reader, perm os.FileMode, maxBytes int64) error {
	if maxBytes <= 0 {
		return fmt.Errorf("invalid maxBytes: %d", maxBytes)
	}
	if err := os.MkdirAll(filepath.Dir(filename), 0750); err != nil {
		return err
	}

	pf, err := renameio.NewPendingFile(filename, renameio.WithPermissions(perm))
	if err != nil {
		return err
	}
	defer pf.Cleanup()

	lr := io.LimitReader(r, maxBytes+1)
	n, err := io.Copy(pf, lr)
	if err != nil {
		return err
	}
	if n > maxBytes {
		return fmt.Errorf("%w: %d bytes exceeds limit %d", ErrFileTooLarge, n, maxBytes)
	}

	if err := pf.CloseAtomicallyReplace(); err != nil {
		return err
	}
	return nil
}

// WriteFile writes the provided content to a local file with specified permissions.
// It ensures that the target directory exists and handles the file writing atomically.
//
// NOTE: we assume the filename is trusted and cleaned without path traversal characters.
func WriteFile(filename string, content []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(filename), 0750); err != nil {
		return err
	}

	return renameio.WriteFile(filename, content, perm)
}
