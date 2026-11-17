package start

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostnode"
)

func TestBootstrapOrdersHooksBeforeWorkerAndState(t *testing.T) {
	t.Parallel()

	gs := &goalstates.MachineGoalState{
		RootFS:    &goalstates.RootFS{MachineDir: t.TempDir()},
		NodeStart: &goalstates.NodeStart{},
	}
	log := slog.New(slog.DiscardHandler)
	saveTask := hostnode.Func("save-daemon-state", func(context.Context) error { return nil })
	name := bootstrapTasks(&config.Config{}, log, "kube1", gs, nil, saveTask).Name()

	hooks := strings.Index(name, "ensure-nspawn-lifecycle-units")
	archives := strings.Index(name, "download-container-image-archives")
	provision := strings.Index(name, "ensure-nspawn-workspace")
	npd := strings.Index(name, "start-npd")
	save := strings.Index(name, "save-daemon-state")
	if hooks < 0 || archives <= hooks || provision <= archives || npd <= provision || save <= npd {
		t.Fatalf("bootstrap task order = %s", name)
	}
}
