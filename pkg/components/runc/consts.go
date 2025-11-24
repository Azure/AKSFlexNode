package runc

// Runc binary paths to check and manage
const (
	PrimaryRuncBinaryPath   = "/usr/bin/runc"
	SecondaryRuncBinaryPath = "/usr/local/bin/runc"
	SbinRuncBinaryPath      = "/usr/sbin/runc"
)

// All possible runc binary locations
var RuncBinaryPaths = []string{
	PrimaryRuncBinaryPath,
	SecondaryRuncBinaryPath,
	SbinRuncBinaryPath,
}
