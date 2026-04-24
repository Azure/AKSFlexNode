// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package goalstates

import (
	"fmt"
	"strings"
	"syscall"
)

// hostKernel returns the running kernel version (equivalent to uname -r).
func hostKernel() (string, error) {
	var utsname syscall.Utsname
	if err := syscall.Uname(&utsname); err != nil {
		return "", fmt.Errorf("uname: %w", err)
	}

	// Utsname.Release is a fixed-size byte array; convert to string and trim
	// the trailing NUL bytes.
	buf := make([]byte, 0, len(utsname.Release))
	for _, b := range utsname.Release {
		if b == 0 {
			break
		}

		buf = append(buf, byte(b))
	}

	return strings.TrimSpace(string(buf)), nil
}
