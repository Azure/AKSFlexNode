package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/Azure/AKSFlexNode/pkg/internal/agentdeployment"
)

// openGateAfterAgentUpgradeJudgment publishes the result of an AgentUpgrade the daemon started
// for, retrying, and then runs proceed, which opens the startup gate, unless the daemon has to be
// replaced first, as after it queued its restart from the host root. It runs nothing while the
// upgrade's signal remains, as when recovery scheduled a restart into the last-good binary,
// which opens the gate instead. Before proceed, it converges the service assets the daemon held
// back at startup, now that the upgrade is judged; failing that is only logged, as the
// last-good version's assets still work, and the next start converges them again.
func openGateAfterAgentUpgradeJudgment(
	ctx context.Context,
	log *slog.Logger,
	publish func(context.Context) error,
	readSignal func() (*agentdeployment.UpgradeSignal, error),
	convergeHeldAssets func(context.Context) error,
	proceed func(),
	retry time.Duration,
) {
	for {
		publishErr := publish(ctx)
		pending, readErr := readSignal()
		switch {
		case publishErr != nil:
			log.Warn("failed to publish AgentUpgrade startup result; will retry", "error", publishErr)
		case readErr != nil:
			log.Warn("failed to confirm AgentUpgrade startup signal; will retry", "error", readErr)
		case pending != nil:
			// Recovery scheduled a restart. Keep reconciliation gated until the
			// last-good process starts and consumes the retained signal.
			return
		default:
			if err := convergeHeldAssets(ctx); err != nil {
				log.Warn("failed to update the agent's service assets after its upgrade; the last-good version's remain",
					"error", err)
			}
			proceed()
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
	}
}
