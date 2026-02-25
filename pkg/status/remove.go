package status

import (
	"errors"
	"os"

	"github.com/sirupsen/logrus"
)

// RemoveStatusFileBestEffort removes the current node status file.
//
// It is intentionally best-effort: failure to remove the file should not crash the agent,
// but it helps ensure subsequent health checks re-collect status from scratch.
func RemoveStatusFileBestEffort(logger *logrus.Logger) {
	RemoveStatusFileBestEffortAtPath(logger, GetStatusFilePath())
}

func RemoveStatusFileBestEffortAtPath(logger *logrus.Logger, statusFilePath string) {
	if logger == nil {
		return
	}
	if statusFilePath == "" {
		logger.Debug("Failed to remove status file: empty path")
		return
	}

	if err := os.Remove(statusFilePath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			logger.Debug("Status file already removed")
			return
		}
		logger.Debugf("Failed to remove status file: %v", err)
		return
	}

	logger.Debug("Removed status file successfully")
}
