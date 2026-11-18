package agentruntime

import (
	"context"
	"fmt"
	"log/slog"
)

func (NSpawn) ValidateBootstrap(_ context.Context, _ *slog.Logger, _ bool) error {
	artifacts, err := inspectBootstrapArtifacts()
	if err != nil {
		return err
	}

	return validateNSpawnBootstrap(artifacts)
}

func validateNSpawnBootstrap(artifacts bootstrapArtifacts) error {
	if artifacts.oci {
		return fmt.Errorf("OCI worker is present; reset before changing isolation")
	}
	if artifacts.none {
		return fmt.Errorf("host-native worker is present; reset before changing isolation")
	}

	return nil
}
