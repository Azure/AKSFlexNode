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
func ResetWorker(log *slog.Logger, worker agentruntime.Runtime, hostHub phases.Task) phases.Task {
	return hostnode.Func("reset-worker", func(ctx context.Context) error {
		plan, err := worker.PlanReset(log)
		if err != nil {
			return err
		}
		tasks := []phases.Task{plan.Stop()}
		// Recovery artifacts can outlive SDK runtime traces. Hub independently
		// checks its own ownership rather than treating those artifacts as proof.
		if plan.HasHostNativeState() {
			tasks = append(tasks, hostHub)
		}
		tasks = append(tasks, plan.Finish())
		return phases.Serial(log, tasks...).Do(ctx)
	})
}
