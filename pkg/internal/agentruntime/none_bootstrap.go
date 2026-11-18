package agentruntime

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/phases/host"
)

func (noneRuntime) ValidateBootstrap(ctx context.Context, log *slog.Logger, installed bool) error {
	artifacts, err := inspectBootstrapArtifacts()
	if err != nil {
		return err
	}
	backend := none.NewDefault(log)
	return validateNoneBootstrap(ctx, log, installed, artifacts, backend, host.EnsureNoExistingDeploymentFor)
}

func validateNoneBootstrap(ctx context.Context, log *slog.Logger, installed bool, artifacts bootstrapArtifacts, backend *none.Backend, ensureClean func(context.Context, *slog.Logger, isolation.Mode) error) error {
	if artifacts.oci {
		return fmt.Errorf("OCI worker is present; reset before changing isolation")
	}
	if !installed && artifacts.none {
		return fmt.Errorf("host-native worker has no applied state; reset before bootstrap")
	}
	if err := ensureClean(ctx, log, isolation.ModeNSpawn); err != nil {
		return err
	}
	return backend.PreflightOwnership()
}
