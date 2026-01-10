package runc

// Runc binary paths to check and manage
const (
	runcBinaryPath = "/usr/bin/runc"
)

var (
	runcFileName    = "runc.%s"
	runcDownloadURL = "https://github.com/opencontainers/runc/releases/download/v%s/" + runcFileName
)
