package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/daemon/internal/provider"
	"github.com/Azure/unbounded/pkg/agent/containerops"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/noneops"
	"github.com/Azure/unbounded/pkg/agent/phases/host"
)

func loadWorkerProvider(ctx context.Context) (provider.Provider, error) {
	store, err := NewFileStateStore()
	if err != nil {
		return nil, err
	}
	state, err := store.Load(ctx)
	if err != nil {
		return nil, err
	}
	mode := isolation.ModeNSpawn
	if state != nil {
		mode = state.Isolation
	}
	return provider.New(mode)
}

// ValidateWorkerBootstrap checks runtime ownership before bootstrap mutates the host.
func ValidateWorkerBootstrap(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	if Containerized() && cfg.Bootstrap.Isolation != isolation.ModeNSpawn {
		return fmt.Errorf("containerized agent does not yet support %s workers; use a host-installed agent", cfg.Bootstrap.Isolation)
	}
	store, err := NewFileStateStore()
	if err != nil {
		return err
	}
	state, err := store.Load(ctx)
	if err != nil {
		return err
	}
	if state != nil && state.Isolation != cfg.Bootstrap.Isolation {
		return fmt.Errorf("worker isolation change requires reset")
	}
	if containerops.Present() && cfg.Bootstrap.Isolation != isolation.ModeContainer {
		return fmt.Errorf("OCI worker is present; reset before changing isolation")
	}
	if cfg.Bootstrap.Isolation == isolation.ModeContainer {
		if noneops.Present() {
			return fmt.Errorf("host-native worker is present; reset before changing isolation")
		}
		return host.EnsureNoExistingDeploymentFor(ctx, log, isolation.ModeNSpawn)
	}
	if cfg.Bootstrap.Isolation == isolation.ModeNone {
		if err := host.EnsureNoExistingDeploymentFor(ctx, log, isolation.ModeNSpawn); err != nil {
			return err
		}
		return none.NewDefault(log).PreflightOwnership()
	}
	if noneops.Present() {
		return fmt.Errorf("host-native worker is present; reset before changing isolation")
	}
	return nil
}
