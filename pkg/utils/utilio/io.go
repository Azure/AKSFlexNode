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

// ReadAll1GiB reads all data from the provided reader, but returns an error if the data exceeds 1 GiB in size.
func ReadAll1GiB(r io.Reader) ([]byte, error) {
	const maxFileSize = 1 * 1024 * 1024 * 1024 // 1 GiB

	lr := io.LimitReader(r, maxFileSize+1) // +1 to detect if file exceeds limited size
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxFileSize {
		return nil, fmt.Errorf("%w: 4 GiB", ErrFileTooLarge)
	}
	return data, nil
}

// InstallFile writes the content from the provided reader to a local file with specified permissions.
// It ensures that the target directory exists and handles the file writing atomically.
func InstallFile(filename string, r io.Reader, perm os.FileMode) error {
	content, err := ReadAll1GiB(r) // TODO: allow configuring max file size
	if err != nil {
		return err
	}

	return WriteFile(filename, content, perm)
}

// WriteFile writes the provided content to a local file with specified permissions.
// It ensures that the target directory exists and handles the file writing atomically.
func WriteFile(filename string, content []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(filename), 0755); err != nil {
		return err
	}

	return renameio.WriteFile(filename, content, perm)
}
