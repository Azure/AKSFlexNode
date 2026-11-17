package daemon

import (
	"context"
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/internal/agentruntime"
	"github.com/Azure/AKSFlexNode/pkg/yurthub"
	"github.com/Azure/unbounded/pkg/agent/hostnode"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

func hubWorker(scope string, worker agentruntime.WorkerContext) yurthub.Worker {
	return yurthub.Worker{
		Scope:       scope,
		Root:        worker.Root,
		Exec:        worker.Exec,
		KubeletUnit: worker.KubeletUnit,
		Mode:        worker.Mode,
	}
}

// ResetHostHub clears host Hub state before none teardown or identity replacement.
func ResetHostHub(log *slog.Logger) phases.Task {
	return hostnode.Func("reset-host-hub", func(ctx context.Context) error {
		return yurthub.ResetHost(ctx, hubWorker("", agentruntime.NoneHost(log)))
	})
}

// ResetWorker coordinates host Hub cleanup before runtime teardown.
func ResetWorker(log *slog.Logger, worker agentruntime.Runtime, mode isolation.Mode) phases.Task {
	if mode != isolation.ModeNone {
		return worker.Reset(log)
	}
	return phases.Serial(log, ResetHostHub(log), worker.Reset(log))
}
