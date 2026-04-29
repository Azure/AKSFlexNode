package status

import (
	"errors"
	"log/slog"
	"os"

	"github.com/Azure/AKSFlexNode/pkg/spec"
)

// RemoveStatusFileBestEffort removes the current node status file.
//
// It is intentionally best-effort: failure to remove the file should not crash the agent,
// but it helps ensure subsequent health checks re-collect status from scratch.
func RemoveStatusFileBestEffort(logger *slog.Logger) {
	RemoveStatusFileBestEffortAtPath(logger, spec.StatusFilePath)
}

func RemoveStatusFileBestEffortAtPath(logger *slog.Logger, statusFilePath string) {
	if statusFilePath == "" {
		logger.Debug("failed to remove status file: empty path")
		return
	}

	if err := os.Remove(statusFilePath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			logger.Debug("status file already removed")
			return
		}
		logger.Debug("failed to remove status file", "error", err)
		return
	}

	logger.Debug("removed status file successfully")
}
