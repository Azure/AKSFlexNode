package config

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"syscall"

	"github.com/Azure/unbounded/pkg/agent/phases"
)

type removeRuntimeDirsTask struct {
	logger *slog.Logger
	paths  []string
	// removeAll is os.RemoveAll, which tests replace to stand in for a mount point.
	removeAll func(string) error
}

func RemoveRuntimeDirs(logger *slog.Logger) phases.Task {
	return &removeRuntimeDirsTask{logger: logger, paths: []string{ConfigDir, DefaultLogDir}}
}

func (t *removeRuntimeDirsTask) Name() string { return "remove-runtime-dirs" }

func (t *removeRuntimeDirsTask) Do(context.Context) error {
	removeAll := t.removeAll
	if removeAll == nil {
		removeAll = os.RemoveAll
	}
	for _, path := range t.paths {
		err := removeAll(path)
		switch {
		case err == nil || errors.Is(err, os.ErrNotExist):
			t.logger.Info("removed runtime directory", "path", path)
		case errors.Is(err, syscall.EBUSY) && emptyDir(path):
			// The directory is a mount point, as in the agent's container, which mounts the
			// host's directories at their own paths. Emptied, it's as removed as it can be.
			t.logger.Info("emptied runtime directory, a mount point", "path", path)
		default:
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}
	return nil
}

func emptyDir(path string) bool {
	entries, err := os.ReadDir(path)
	return err == nil && len(entries) == 0
}
