//go:build !linux

package v20260301

import "fmt"

// unmountBelow is not supported on non-Linux platforms because it
// requires /proc/mounts and the syscall.Unmount syscall.
func unmountBelow(root string) error {
	return fmt.Errorf("unmount not supported on this platform")
}
