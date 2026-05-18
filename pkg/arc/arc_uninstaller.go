package arc

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// uninstallArcTask implements phases.Task for local Arc cleanup during reset.
type uninstallArcTask struct {
	logger *slog.Logger
}

// UninstallArc returns a phases.Task that detects local Azure Arc agent state,
// disconnects the agent when possible, and removes Arc binaries and config.
//
// The task uses best-effort semantics: individual cleanup steps log warnings
// on failure but do not abort reset, so that as much cleanup as possible is
// performed.
func UninstallArc(logger *slog.Logger) phases.Task {
	return &uninstallArcTask{logger: logger}
}

func (t *uninstallArcTask) Name() string { return "uninstall-arc" }

func (t *uninstallArcTask) Do(ctx context.Context) error {
	if !isArcDetectedOnHost(ctx, t.logger) {
		t.logger.Info("Azure Arc not detected on host, skipping uninstall")
		return nil
	}

	t.logger.Info("starting local Arc cleanup")

	var failedOps []string
	if err := t.disconnectArcMachine(ctx); err != nil {
		t.logger.Warn("failed to disconnect Arc machine (continuing cleanup)", "error", err)
		failedOps = append(failedOps, "Arc machine disconnection")
	}

	if err := t.removeArcAgentLocalState(ctx); err != nil {
		t.logger.Warn("failed to remove Arc agent local state (continuing cleanup)", "error", err)
		failedOps = append(failedOps, "Arc agent local state removal")
	}

	if len(failedOps) > 0 {
		t.logger.Warn("Arc cleanup completed with failures",
			"failedCount", len(failedOps),
			"failedOps", strings.Join(failedOps, ", "))
		return nil
	}

	t.logger.Info("Arc cleanup completed successfully")
	return nil
}

func isArcDetectedOnHost(ctx context.Context, logger *slog.Logger) bool {
	if isArcAgentInstalled() {
		return true
	}

	for _, service := range arcServices {
		if utilexec.ServiceExists(ctx, logger, service) || utilexec.IsServiceActive(ctx, logger, service) {
			return true
		}
	}

	for _, path := range append(append([]string{}, arcBinaryPaths...), append(arcDirectories, arcServiceFiles...)...) {
		if pathExists(path) {
			return true
		}
	}

	return false
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (t *uninstallArcTask) disconnectArcMachine(ctx context.Context) error {
	if !isArcAgentInstalled() {
		t.logger.Info("Azure Arc agent binary not found, skipping disconnect")
		return nil
	}
	if err := utilexec.RunCmd(ctx, t.logger, utilexec.Azcmagent(), "disconnect", "--force-local-only"); err != nil {
		return fmt.Errorf("disconnect Arc machine: %w", err)
	}
	t.logger.Info("Arc machine disconnected")
	return nil
}

func (t *uninstallArcTask) removeArcAgentLocalState(ctx context.Context) error {
	var removalErrors []string

	for _, service := range arcServices {
		if !utilexec.ServiceExists(ctx, t.logger, service) && !utilexec.IsServiceActive(ctx, t.logger, service) {
			continue
		}
		if err := utilexec.StopService(ctx, t.logger, service); err != nil {
			t.logger.Debug("failed to stop service", "service", service, "error", err)
			removalErrors = append(removalErrors, fmt.Sprintf("stop %s: %v", service, err))
		}
		if err := utilexec.DisableService(ctx, t.logger, service); err != nil {
			t.logger.Debug("failed to disable service", "service", service, "error", err)
			removalErrors = append(removalErrors, fmt.Sprintf("disable %s: %v", service, err))
		}
	}

	for _, path := range arcBinaryPaths {
		if err := utilexec.RemoveFileIfExists(path); err != nil {
			t.logger.Debug("failed to remove binary", "path", path, "error", err)
			removalErrors = append(removalErrors, err.Error())
		}
	}

	for _, dir := range arcDirectories {
		if err := utilexec.RemoveAllIfExists(dir); err != nil {
			t.logger.Debug("failed to remove directory", "dir", dir, "error", err)
			removalErrors = append(removalErrors, err.Error())
		}
	}

	for _, file := range arcServiceFiles {
		if err := utilexec.RemoveFileIfExists(file); err != nil {
			t.logger.Debug("failed to remove service file", "file", file, "error", err)
			removalErrors = append(removalErrors, err.Error())
		}
	}

	if err := utilexec.ReloadSystemd(ctx, t.logger); err != nil {
		t.logger.Debug("failed to reload systemd daemon", "error", err)
		removalErrors = append(removalErrors, fmt.Sprintf("reload systemd: %v", err))
	}

	if isArcDetectedOnHost(ctx, t.logger) {
		removalErrors = append(removalErrors, "Arc artifacts still present after cleanup")
	}

	if len(removalErrors) > 0 {
		return fmt.Errorf("local Arc cleanup errors: %s", strings.Join(removalErrors, "; "))
	}

	t.logger.Info("Azure Arc agent binaries and configuration removed")
	return nil
}
