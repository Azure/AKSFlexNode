// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package utilio

import "os"

func IsExecutable(filePath string) bool {
	s, err := os.Stat(filePath)
	if err != nil {
		return false
	}

	return !s.IsDir() && s.Mode()&0o111 != 0
}
