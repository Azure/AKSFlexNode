package utilio

import (
	"errors"
	"os"
	"path/filepath"
)

// SyncDir persists changes to the directory's entries.
func SyncDir(path string) error {
	f, err := os.Open(path) //nolint:gosec // callers own the directory whose entries are persisted
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// WriteFileDurable publishes atomically and persists the file's name and any
// parent directories it creates. Journals must survive loss of power once a
// caller starts the mutation they describe.
func WriteFileDurable(path string, data []byte, mode os.FileMode) error {
	var parents []string
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		_, err := os.Stat(dir)
		parents = append(parents, dir)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := WriteFile(path, data, mode); err != nil {
		return err
	}
	for _, dir := range parents {
		if err := SyncDir(dir); err != nil {
			return err
		}
	}
	return nil
}
