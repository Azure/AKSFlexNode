package bootstrapper

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// ---------------------------------------------------------------------------
// Enrich Cluster Config
// ---------------------------------------------------------------------------

type enrichClusterConfigTask struct {
	cfg    *config.Config
	logger *slog.Logger
}

// EnrichClusterConfig returns a task that fetches cluster metadata (server URL,
// CA cert) from AKS for non-bootstrap-token auth modes.
func EnrichClusterConfig(cfg *config.Config, logger *slog.Logger) phases.Task {
	return &enrichClusterConfigTask{cfg: cfg, logger: logger}
}

func (t *enrichClusterConfigTask) Name() string { return "enrich-cluster-config" }

func (t *enrichClusterConfigTask) Do(ctx context.Context) error {
	enricher := newClusterConfigEnricher(t.cfg, toLogrus(t.logger))
	if enricher.IsCompleted(ctx) {
		return nil
	}
	return enricher.Execute(ctx)
}

// ---------------------------------------------------------------------------
// Install Binary (copy aks-flex-node into nspawn rootfs)
// ---------------------------------------------------------------------------

type installBinaryTask struct {
	machineDir string
}

// InstallBinary returns a task that copies the current process binary into
// the nspawn rootfs at /usr/local/bin/aks-flex-node.
func InstallBinary(machineDir string) phases.Task {
	return &installBinaryTask{machineDir: machineDir}
}

func (t *installBinaryTask) Name() string { return "install-binary-in-rootfs" }

func (t *installBinaryTask) Do(_ context.Context) error {
	selfPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve self executable: %w", err)
	}

	destPath := filepath.Join(t.machineDir, "usr", "local", "bin", "aks-flex-node")
	if err := os.MkdirAll(filepath.Dir(destPath), 0o750); err != nil { //nolint:gosec // directory needs to be traversable
		return fmt.Errorf("create destination directory: %w", err)
	}

	src, err := os.Open(selfPath) //nolint:gosec // path is from os.Executable(), not user input
	if err != nil {
		return fmt.Errorf("open self binary: %w", err)
	}
	defer func() { _ = src.Close() }()

	dst, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o750) //nolint:gosec // binary must be executable
	if err != nil {
		return fmt.Errorf("create destination binary: %w", err)
	}
	defer func() { _ = dst.Close() }()

	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("copy binary: %w", err)
	}

	return nil
}
