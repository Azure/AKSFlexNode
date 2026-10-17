package daemon

import (
	"os"
	"strconv"
)

// ContainerizedEnv marks the agent as running in its container image, which sets it. There, the
// image and the container's systemd unit stand in for what start otherwise installs on the host:
// OS packages, and the agent's own binary and unit.
const ContainerizedEnv = "AKS_FLEX_NODE_CONTAINERIZED"

// Containerized reports whether the agent runs in its container image.
func Containerized() bool {
	return containerized(os.Getenv)
}

func containerized(getenv func(string) string) bool {
	on, err := strconv.ParseBool(getenv(ContainerizedEnv))
	return err == nil && on
}
