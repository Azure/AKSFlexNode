package daemon

import (
	"context"
	"log/slog"
)

func (*fakeWorkerRuntime) ValidateBootstrap(context.Context, *slog.Logger, bool) error {
	return nil
}
