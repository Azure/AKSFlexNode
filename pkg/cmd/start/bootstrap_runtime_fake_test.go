package start

import (
	"context"
	"log/slog"
)

func (*bootstrapTestRuntime) ValidateBootstrap(context.Context, *slog.Logger, bool) error {
	return nil
}
