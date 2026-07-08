//go:build !local_e2e

package aksmachine

import (
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/config"
)

// NewMachineClient creates a MachineClient instance from config.
func NewMachineClient(cfg *config.Config, logger *slog.Logger) (MachineClient, error) {
	return newMachineClientFromConfig(cfg, logger)
}
