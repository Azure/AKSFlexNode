package utilhost

import (
	"fmt"
	"runtime"
)

var (
	arch        string // arm64 / amd64
	machineArch string // aarch64 / x86_64
)

func init() {
	switch runtime.GOARCH {
	case "amd64":
		arch = "amd64"
		machineArch = "x86_64"
	case "arm64":
		arch = "arm64"
		machineArch = "aarch64"
	default:
		panic(fmt.Sprintf("unsupported architecture: %s", runtime.GOARCH))
	}
}

// GetArch returns the architecture of the host with format like "amd64", "arm64".
func GetArch() string {
	return arch
}

// GetMachineArch returns the machine architecture of the host with format like "x86_64", "aarch64".
func GetMachineArch() string {
	return machineArch
}
