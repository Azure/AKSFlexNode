package agentruntime

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestRestartStagesArchivesBeforeStoppingAndRestartsNPD(t *testing.T) {
	t.Parallel()

	gs := &goalstates.MachineGoalState{
		RootFS:    &goalstates.RootFS{MachineDir: t.TempDir()},
		NodeStart: &goalstates.NodeStart{},
	}
	name := (NSpawn{}).Restart(&config.Config{}, slog.New(slog.DiscardHandler), "kube1", gs, nil).Name()

	previous := -1
	for _, task := range []string{"download-container-image-archives", "stop-node", "start-nspawn-machine", "wait-for-kubelet", "start-npd"} {
		index := strings.Index(name, task)
		if index < 0 || index <= previous {
			t.Fatalf("restart task order = %s", name)
		}
		previous = index
	}
}
