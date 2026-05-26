package aksmachine

import (
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/config"
)

func newMachineClientFromConfig(cfg *config.Config, logger *slog.Logger) (MachineClient, error) {
	if cfg.Agent.ARMProxyURLOverrideForE2E != "" {
		return newARMProxyClient(cfg, logger)
	}
	return newARMClient(cfg, logger)
}
