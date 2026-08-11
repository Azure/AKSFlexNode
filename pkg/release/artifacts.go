// Package release defines the artifact naming contract shared by Flex Node
// release producers and consumers.
package release

import "fmt"

const AgentBinaryBaseName = "aks-flex-node"

// AgentBinaryArchiveMember returns the binary member name used by a release
// archive for the requested platform.
func AgentBinaryArchiveMember(goos, goarch string) (string, error) {
	if goos != "linux" {
		return "", fmt.Errorf("unsupported agent release operating system %q", goos)
	}
	switch goarch {
	case "amd64", "arm64":
		return AgentBinaryBaseName + "-" + goos + "-" + goarch, nil
	default:
		return "", fmt.Errorf("unsupported agent release architecture %q", goarch)
	}
}
