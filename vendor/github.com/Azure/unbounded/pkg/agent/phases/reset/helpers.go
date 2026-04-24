// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package reset

import (
	"errors"
	"log/slog"
	"os"
)

// removeFileIfExists removes a file if it exists. Non-ENOENT errors are
// logged at Warn so we have a trace but don't abort the reset flow.
func removeFileIfExists(log *slog.Logger, path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn("failed to remove file", "path", path, "error", err)
	}
}

// removeAllIfExists removes a path and all children if it exists. Errors are
// logged at Warn so we have a trace but don't abort the reset flow.
func removeAllIfExists(log *slog.Logger, path string) {
	if err := os.RemoveAll(path); err != nil {
		log.Warn("failed to remove directory", "path", path, "error", err)
	}
}
