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
	"github.com/Azure/unbounded/pkg/agent/artifactsource"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

const (
	DefaultVersion = "v1.35.1"

	defaultNPDURLTemplate = "https://github.com/kubernetes/node-problem-detector/releases/download/%s/node-problem-detector-%s-linux_%s.tar.gz"

	// Paths as they appear inside the container.
	npdBinaryPath = "/usr/bin/node-problem-detector"
	npdConfigPath = "/etc/node-problem-detector/kernel-monitor.json"

	npdArtifactCheckName = "npd-artifact"
	npdArtifactTarget    = "node-problem-detector artifact"
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
		version = DefaultVersion
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

// Preflight returns AKS Flex Node-specific preflight checks.
func Preflight(cfg *config.Config) []preflight.Checker {
	return []preflight.Checker{
		artifactsource.ReachabilityChecker{
			CheckName:  npdArtifactCheckName,
			Target:     npdArtifactTarget,
			OKMessage:  "node-problem-detector artifact is reachable",
			ErrMessage: "node-problem-detector artifact is not reachable",
			Sources: func() (artifactsource.Sources, error) {
				version := DefaultVersion
				if cfg != nil && cfg.Npd.Version != "" {
					version = cfg.Npd.Version
				}

				source, err := artifactsource.Parse(constructDownloadURL(version))
				if err != nil {
					return nil, err
				}

				return artifactsource.Sources{"node-problem-detector": source}, nil
			},
		},
	}
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
