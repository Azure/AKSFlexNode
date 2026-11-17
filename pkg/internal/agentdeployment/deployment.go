package agentdeployment

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

// ContainerizedEnv marks the agent as running in its container image.
const ContainerizedEnv = "AKS_FLEX_NODE_CONTAINERIZED"

// Containerized reports whether the agent runs in its container image.
func Containerized() bool {
	return containerized(os.Getenv)
}

func containerized(getenv func(string) string) bool {
	on, err := strconv.ParseBool(getenv(ContainerizedEnv))
	return err == nil && on
}

// CredentialPlugin is the agent binary used by its Kubernetes clients when
// managed identity or a service principal supplies bootstrap credentials.
func CredentialPlugin() (string, error) {
	return credentialPlugin(Containerized(), RunningAgentExecutable, config.HostBinaryPath)
}

func credentialPlugin(containerized bool, runningExecutable func() (string, error), hostBinary func() string) (string, error) {
	if !containerized {
		return hostBinary(), nil
	}
	return runningExecutable()
}

// RunningAgentExecutable resolves the executable of this process, following symlinks.
func RunningAgentExecutable() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve running agent executable: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve running agent executable symlinks: %w", err)
	}
	return resolved, nil
}

// These host checks do not apply to the agent in its container: packages are
// supplied by host provisioning, and containerd runs the agent itself.
var containerSkippedPreflightChecks = []string{"host-packages", "containerd-active"}

// PreflightChecks omits host checks that do not apply in the agent container.
func PreflightChecks(checks []preflight.Checker) []preflight.Checker {
	return preflightChecks(checks, Containerized())
}

func preflightChecks(checks []preflight.Checker, containerized bool) []preflight.Checker {
	if !containerized {
		return checks
	}
	return slices.DeleteFunc(slices.Clone(checks), func(check preflight.Checker) bool {
		return slices.Contains(containerSkippedPreflightChecks, check.Name())
	})
}
