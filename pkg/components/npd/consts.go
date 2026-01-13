package npd

// NPD binary paths to check and manage
const (
	PrimaryNpdBinaryPath   = "/usr/bin/node-problem-detector"
	SecondaryNpdBinaryPath = "/usr/local/bin/node-problem-detector"
	SbinNpdBinaryPath      = "/usr/sbin/node-problem-detector"
)

// All possible NPD binary locations
var NpdBinaryPaths = []string{
	PrimaryNpdBinaryPath,
	SecondaryNpdBinaryPath,
	SbinNpdBinaryPath,
}
