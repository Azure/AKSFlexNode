package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"github.com/Azure/AKSFlexNode/pkg/yurthub"
)

func clearHostHub(ctx context.Context, log *slog.Logger) error {
	if err := yurthub.ResetHost(ctx, log); err != nil {
		return err
	}
	if err := os.Remove("/" + hubWorkerJournal); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

type clearHostHubTask struct{ log *slog.Logger }

func (*clearHostHubTask) Name() string                   { return "clear-host-hub" }
func (t *clearHostHubTask) Do(ctx context.Context) error { return clearHostHub(ctx, t.log) }
