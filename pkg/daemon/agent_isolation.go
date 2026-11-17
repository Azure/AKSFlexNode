package daemon

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// checkAgentIsolation uses Unbounded's existing candidate probe. Nspawn keeps
// accepting releases that predate the probe; new worker modes require it.
func checkAgentIsolation(ctx context.Context, path string, mode isolation.Mode) error {
	if err := config.ValidateIsolation(mode); err != nil {
		return err
	}
	if mode == isolation.ModeNSpawn {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, path, "supports-isolation", string(mode)).Run(); err != nil { // #nosec G204 -- verified candidate and fixed protocol arguments
		return fmt.Errorf("candidate cannot operate %s workers: %w", mode, err)
	}
	return nil
}
