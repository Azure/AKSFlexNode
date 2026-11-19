package daemon

import (
	"context"
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/internal/agentruntime"
	"github.com/Azure/AKSFlexNode/pkg/yurthub"
	"github.com/Azure/unbounded/pkg/agent/hostnode"
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

// ResetWorker discovers ownership at execution time, after CLI daemon shutdown.
func ResetWorker(log *slog.Logger, worker agentruntime.Runtime, hostHub phases.Task) phases.Task {
	return hostnode.Func("reset-worker", func(ctx context.Context) error {
		plan := worker.PlanReset(log)

		tasks := []phases.Task{plan.Stop()}
		if plan.HasHostNativeState() {
			tasks = append(tasks, hostHub)
		}
		tasks = append(tasks, plan.Finish())

		return phases.Serial(log, tasks...).Do(ctx)
	})
}
