// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// AppliedConfigChecksumPath returns the path to the SHA-256 sidecar file
// for the given nspawn machine's applied config, e.g.
// /etc/unbounded/agent/kube1-applied-config.json.sha256.
func AppliedConfigChecksumPath(machineName string) string {
	return AppliedConfigPath(machineName) + ".sha256"
}

// ComputeChecksum returns the lowercase hex-encoded SHA-256 digest of data.
func ComputeChecksum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// ErrChecksumMismatch is returned when the sidecar checksum does not match
// the config file content, indicating possible on-disk corruption.
var ErrChecksumMismatch = errors.New("applied config checksum mismatch")

// VerifyChecksum compares the SHA-256 digest of data against the content of
// the sidecar checksum file at checksumPath.
//
// Integrity assumptions:
//   - Each file (config and sidecar) is written atomically via renameio,
//     so an individual file is never half-written.
//   - A missing sidecar is not an error: the config may have been written
//     by an older agent version that did not produce checksums, or the
//     sidecar write may not have completed before a crash. In this case
//     VerifyChecksum returns nil so the caller can proceed.
//   - A present sidecar whose digest does not match the config content
//     indicates on-disk corruption (e.g. bitflip). This returns
//     ErrChecksumMismatch.
func VerifyChecksum(data []byte, checksumPath string) error {
	stored, err := os.ReadFile(checksumPath)
	if errors.Is(err, os.ErrNotExist) {
		// No sidecar on disk - nothing to verify.
		return nil
	}

	if err != nil {
		return fmt.Errorf("read checksum file %s: %w", checksumPath, err)
	}

	expected := strings.TrimSpace(string(stored))
	actual := ComputeChecksum(data)

	if expected != actual {
		return fmt.Errorf("%w: file %s expected %s got %s", ErrChecksumMismatch, checksumPath, expected, actual)
	}

	return nil
}
