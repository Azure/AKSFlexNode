package agentdeployment

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// SnapshotCandidate makes compatibility probes and SDK activation consume the same
// private input even if the caller replaces the original candidate after preflight.
func SnapshotCandidate(path string) (string, func(), error) {
	candidate, err := resolveExecutable(path)
	if err != nil {
		return "", nil, fmt.Errorf("resolve activation candidate: %w", err)
	}

	dir, err := os.MkdirTemp("", "flex-activation-*")
	if err != nil {
		return "", nil, fmt.Errorf("create activation snapshot: %w", err)
	}
	cleanup := func() {
		_ = os.RemoveAll(dir)
	}
	path = filepath.Join(dir, "candidate")
	if err := copyExecutable(candidate, path); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("snapshot activation candidate: %w", err)
	}

	return path, cleanup, nil
}

// CheckIsolation probes a verified private candidate before it can replace the agent.
func CheckIsolation(ctx context.Context, path string, mode isolation.Mode) error {
	if err := config.ValidateIsolation(mode); err != nil {
		return err
	}
	if mode == isolation.ModeNSpawn {
		// Nspawn candidates can predate the compatibility probe.
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, path, "supports-isolation", string(mode)).Run(); err != nil { // #nosec G204 -- verified candidate and fixed arguments
		return fmt.Errorf("candidate cannot operate %s workers: %w", mode, err)
	}
	return nil
}
