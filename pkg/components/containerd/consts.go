package containerd

import (
	"strconv"
	"strings"
)

const (
	systemBinDir               = "/usr/bin"
	defaultContainerdBinaryDir = "/usr/bin/containerd"
	defaultContainerdConfigDir = "/etc/containerd"
	containerdConfigFile       = "/etc/containerd/config.toml"
	containerdServiceFile      = "/etc/systemd/system/containerd.service"
	containerdDataDir          = "/var/lib/containerd"
)

var containerdDirs = []string{
	defaultContainerdConfigDir,
}

// containerdV1Binaries lists all binaries included in containerd 1.x releases
var containerdV1Binaries = []string{
	"ctr",
	"containerd",
	"containerd-shim",
	"containerd-shim-runc-v1",
	"containerd-shim-runc-v2",
	"containerd-stress",
}

// containerdV2Binaries lists all binaries included in containerd 2.x releases
// Note: containerd-shim and containerd-shim-runc-v1 were removed in v2.x
var containerdV2Binaries = []string{
	"ctr",
	"containerd",
	"containerd-shim-runc-v2",
	"containerd-stress",
}

// getAllContainerdBinaries returns all possible containerd binaries across all versions
// This is useful for cleanup operations that need to remove binaries from any version
func getAllContainerdBinaries() []string {
	seen := make(map[string]bool)
	var all []string

	for _, binary := range containerdV1Binaries {
		if !seen[binary] {
			all = append(all, binary)
			seen[binary] = true
		}
	}

	for _, binary := range containerdV2Binaries {
		if !seen[binary] {
			all = append(all, binary)
			seen[binary] = true
		}
	}

	return all
}

var (
	containerdFileName    = "containerd-%s-linux-%s.tar.gz"
	containerdDownloadURL = "https://github.com/containerd/containerd/releases/download/v%s/" + containerdFileName
)

// getContainerdBinariesForVersion returns the list of binaries that should exist
// for the given containerd version. In containerd 2.x, containerd-shim and
// containerd-shim-runc-v1 are removed.
func getContainerdBinariesForVersion(version string) []string {
	majorVersion := getMajorVersion(version)

	if majorVersion >= 2 {
		return containerdV2Binaries
	}

	return containerdV1Binaries
}

// getMajorVersion extracts the major version number from a version string
// Examples: "1.7.20" -> 1, "2.0.0" -> 2
func getMajorVersion(version string) int {
	parts := strings.Split(version, ".")
	if len(parts) == 0 {
		return 0
	}

	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0
	}

	return major
}
