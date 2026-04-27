package npd

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	utilexec "k8s.io/utils/exec"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilhost"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

const (
	defaultNPDURLTemplate = "https://github.com/kubernetes/node-problem-detector/releases/download/%s/node-problem-detector-%s-linux_%s.tar.gz"

	// Paths as they appear inside the container.
	npdBinaryPath = "/usr/bin/node-problem-detector"
	npdConfigPath = "/etc/node-problem-detector/kernel-monitor.json"
)

type downloadTask struct {
	version    string
	machineDir string
}

// Download returns a task that downloads the node-problem-detector binary
// and config from the upstream GitHub release tarball into the nspawn
// machine rootfs at machineDir.
func Download(cfg *config.Config, machineDir string) phases.Task {
	version := cfg.Npd.Version
	if version == "" {
		version = config.DefaultNPDVersion
	}
	return &downloadTask{version: version, machineDir: machineDir}
}

func (t *downloadTask) Name() string { return "download-npd" }

func (t *downloadTask) Do(ctx context.Context) error {
	hostBinaryPath := filepath.Join(t.machineDir, npdBinaryPath)
	hostConfigPath := filepath.Join(t.machineDir, npdConfigPath)

	if versionMatch(hostBinaryPath, t.version) {
		return nil // already installed at correct version
	}

	downloadURL := constructDownloadURL(t.version)
	for tarFile, err := range utilio.DecompressTarGzFromRemote(ctx, downloadURL) {
		if err != nil {
			return fmt.Errorf("decompress npd tar: %w", err)
		}

		switch tarFile.Name {
		case "bin/node-problem-detector":
			if err := utilio.InstallFile(hostBinaryPath, tarFile.Body, 0o755); err != nil { //nolint:gosec // binary must be executable
				return fmt.Errorf("install npd binary: %w", err)
			}
		case "config/kernel-monitor.json":
			if err := utilio.InstallFile(hostConfigPath, tarFile.Body, 0o644); err != nil { //nolint:gosec // config must be readable
				return fmt.Errorf("install npd config: %w", err)
			}
		default:
			continue
		}
	}

	return nil
}

func constructDownloadURL(version string) string {
	arch := utilhost.GetArch()
	return fmt.Sprintf(defaultNPDURLTemplate, version, version, arch)
}

func versionMatch(hostBinaryPath, expectedVersion string) bool {
	if !utilio.IsExecutable(hostBinaryPath) {
		return false
	}
	output, err := utilexec.New().Command(hostBinaryPath, "--version").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(output), expectedVersion)
}
