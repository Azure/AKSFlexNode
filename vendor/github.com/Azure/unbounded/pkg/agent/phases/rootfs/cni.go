// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/utilexec"
	"github.com/Azure/unbounded/pkg/agent/utilio"
)

const (
	// cniBinDir is the standard CNI binary directory relative to the machine root.
	cniBinDir = "opt/cni/bin"

	// cniPluginsURLTemplate is the download URL template for CNI plugins.
	// Parameters: version, arch, version.
	cniPluginsURLTemplate = "https://github.com/containernetworking/plugins/releases/download/v%s/cni-plugins-linux-%s-v%s.tgz"
)

// requiredCNIPlugins lists the CNI plugins that must be present for a valid installation.
var requiredCNIPlugins = []string{
	"bridge",
	"host-local",
	"loopback",
}

type downloadCNIBinaries struct {
	log       *slog.Logger
	goalState *goalstates.RootFS
}

// DownloadCNIBinaries returns a task that downloads and installs CNI plugin binaries into the rootfs.
// It skips the download if all required plugins are already installed and the version matches.
func DownloadCNIBinaries(log *slog.Logger, goalState *goalstates.RootFS) phases.Task {
	return &downloadCNIBinaries{log: log, goalState: goalState}
}

func (d *downloadCNIBinaries) Name() string { return "download-cni-binaries" }

func (d *downloadCNIBinaries) Do(ctx context.Context) error {
	destDir := filepath.Join(d.goalState.MachineDir, cniBinDir)
	downloadURL := fmt.Sprintf(cniPluginsURLTemplate, d.goalState.CNIPluginVersion, d.goalState.HostArch, d.goalState.CNIPluginVersion)

	if hasRequiredCNIPlugins(destDir) && cniPluginsVersionMatch(ctx, d.log, destDir, d.goalState.CNIPluginVersion) {
		return nil
	}

	for tarFile, err := range utilio.DecompressTarGzFromRemote(ctx, downloadURL) {
		if err != nil {
			return fmt.Errorf("decompress CNI plugins tar: %w", err)
		}

		targetFilePath := filepath.Join(destDir, tarFile.Name)

		if err := utilio.InstallFile(targetFilePath, tarFile.Body, 0o755); err != nil {
			return fmt.Errorf("install CNI plugin %q: %w", targetFilePath, err)
		}
	}

	return nil
}

// hasRequiredCNIPlugins checks if all required CNI plugins are installed and executable.
func hasRequiredCNIPlugins(cniBinPath string) bool {
	for _, plugin := range requiredCNIPlugins {
		pluginPath := filepath.Join(cniBinPath, plugin)
		if !utilio.IsExecutable(pluginPath) {
			return false
		}
	}

	return true
}

// cniPluginsVersionMatch checks if the installed CNI plugins version matches the expected version.
// It uses the loopback plugin as the version check reference, as it is always present.
func cniPluginsVersionMatch(ctx context.Context, log *slog.Logger, cniBinPath, expectedVersion string) bool {
	loopbackPath := filepath.Join(cniBinPath, "loopback")
	if !utilio.IsExecutable(loopbackPath) {
		return false
	}

	output, err := utilexec.OutputCmd(ctx, log, loopbackPath, "--version")
	if err != nil {
		// Some CNI plugin versions don't support --version; treat as not matching.
		return false
	}

	return strings.Contains(output, expectedVersion)
}
