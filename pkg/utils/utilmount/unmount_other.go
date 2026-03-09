//go:build !linux

package utilmount

import "fmt"

// UnmountBelow is not supported on non-Linux platforms because it
// requires /proc/self/mountinfo and the unix.Unmount syscall.
func UnmountBelow(root string) error {
	return fmt.Errorf("unmount not supported on this platform")
}
