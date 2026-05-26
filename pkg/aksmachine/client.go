package aksmachine

import (
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/config"
)

func newMachineClientFromConfig(cfg *config.Config, logger *slog.Logger) (MachineClient, error) {
	// TODO: support overriding arm endpoint
	return newARMClient(cfg, logger)
}
