package runtimeprovider_test

import (
	"log/slog"
	"slices"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/internal/runtimeprovider"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

// TestNspawnLifecycleTasksInstallTheHelperFirst: the move copies the helper an
// earlier release installed, which regenerates the machine's units with a path
// under /usr/local that the move removes. Rewriting the units has to replace it
// with this release's helper before the units name it, or the machine does not
// start again after its next restart.
func TestNspawnLifecycleTasksInstallTheHelperFirst(t *testing.T) {
	t.Parallel()
	var names []string
	for _, task := range runtimeprovider.NSpawnLifecycleTasks(slog.New(slog.DiscardHandler), &goalstates.RootFS{}) {
		names = append(names, task.Name())
	}
	if want := []string{"ensure-nspawn-lifecycle-helper", "ensure-nspawn-config"}; !slices.Equal(names, want) {
		t.Fatalf("nspawnLifecycleTasks() = %v, want %v", names, want)
	}
}
