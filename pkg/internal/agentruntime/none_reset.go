package agentruntime

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/npd"
	"github.com/Azure/unbounded/pkg/agent/hostnode"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/noneops"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type noneTeardown struct {
	log *slog.Logger
	sdk *noneops.Teardown
}

func (t *noneTeardown) Present() bool { return t.sdk.Present() }

func (t *noneTeardown) Stop() phases.Task { return t.sdk.Stop() }

func (t *noneTeardown) Destroy() phases.Task {
	return hostnode.Func("reset-none-node", func(ctx context.Context) error {
		if err := npd.RemoveHost(t.log, none.Root{HostRoot: t.sdk.Root}).Do(ctx); err != nil {
			return err
		}

		if err := t.sdk.RemoveConfig().Do(ctx); err != nil {
			return fmt.Errorf("remove host-native configuration: %w", err)
		}

		if err := t.sdk.Destroy().Do(ctx); err != nil {
			return fmt.Errorf("destroy host-native worker: %w", err)
		}

		if err := hostnode.ResetCNIState(t.sdk.Root); err != nil {
			return fmt.Errorf("clean host-native CNI state: %w", err)
		}

		return nil
	})
}
