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

// ResetHostHub clears host Hub state before none teardown or identity replacement.
func ResetHostHub(log *slog.Logger) phases.Task {
	return hostnode.Func("reset-host-hub", func(ctx context.Context) error {
		return yurthub.ResetHost(ctx, hubWorker("", agentruntime.NoneHost(log)))
	})
}

// ResetWorker discovers ownership at execution time, after CLI daemon shutdown.
func ResetWorker(log *slog.Logger, worker agentruntime.Runtime) phases.Task {
	return hostnode.Func("reset-worker", func(ctx context.Context) error {
		return runResetPlan(ctx, log, worker.PlanReset(log), ResetHostHub(log))
	})
}

func runResetPlan(ctx context.Context, log *slog.Logger, plan agentruntime.ResetPlan, hub phases.Task) error {
	tasks := []phases.Task{plan.Stop()}
	if plan.HasNone() {
		tasks = append(tasks, hub)
	}
	tasks = append(tasks, plan.Finish())
	return phases.Serial(log, tasks...).Do(ctx)
}
