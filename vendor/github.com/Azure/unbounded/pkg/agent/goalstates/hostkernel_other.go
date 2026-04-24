// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build !linux

package goalstates

import "fmt"

// hostKernel is not supported on non-Linux platforms.
func hostKernel() (string, error) {
	return "", fmt.Errorf("hostKernel is only supported on Linux")
}
