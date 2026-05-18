package config

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/Azure/unbounded/pkg/agent/phases"
)

type removeRuntimeDirsTask struct {
	logger *slog.Logger
	paths  []string
}

func RemoveRuntimeDirs(logger *slog.Logger) phases.Task {
	return &removeRuntimeDirsTask{logger: logger, paths: []string{RuntimeDir, ConfigDir, DefaultLogDir}}
}

func (t *removeRuntimeDirsTask) Name() string { return "remove-runtime-dirs" }

func (t *removeRuntimeDirsTask) Do(context.Context) error {
	for _, path := range t.paths {
		if err := os.RemoveAll(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
		t.logger.Info("removed runtime directory", "path", path)
	}
	return nil
}
