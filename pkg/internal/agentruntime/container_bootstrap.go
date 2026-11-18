package agentruntime

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/phases/host"
)

func (containerRuntime) ValidateBootstrap(ctx context.Context, log *slog.Logger, installed bool) error {
	artifacts, err := inspectBootstrapArtifacts()
	if err != nil {
		return err
	}
	return validateContainerBootstrap(ctx, log, installed, artifacts, host.EnsureNoExistingDeploymentFor)
}

func validateContainerBootstrap(ctx context.Context, log *slog.Logger, installed bool, artifacts bootstrapArtifacts, ensureClean func(context.Context, *slog.Logger, isolation.Mode) error) error {
	if artifacts.none {
		return fmt.Errorf("host-native worker is present; reset before changing isolation")
	}
	if !installed && artifacts.oci {
		return fmt.Errorf("OCI worker has no applied state; reset before bootstrap")
	}
	if installed {
		// An installed OCI worker owns its bpffs paths; they are not conflicts.
		return nil
	}
	return ensureClean(ctx, log, isolation.ModeNSpawn)
}
