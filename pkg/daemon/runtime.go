package daemon

import (
	"context"
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/config"
)

type machineGoalResolver func(context.Context, *slog.Logger, *config.Config, string) (config.ResolvedMachineGoal, error)
