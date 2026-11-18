package daemon

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/internal/agentruntime"
	"github.com/Azure/unbounded/pkg/agent/hostnode"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type testResetPlan struct {
	hostState bool
	stop      phases.Task
	finish    phases.Task
}

func (p testResetPlan) HasHostNativeState() bool { return p.hostState }
func (p testResetPlan) Stop() phases.Task        { return p.stop }
func (p testResetPlan) Finish() phases.Task      { return p.finish }

type testResetRuntime struct {
	agentruntime.Runtime
	plan agentruntime.ResetPlan
}

func (r testResetRuntime) PlanReset(*slog.Logger) agentruntime.ResetPlan {
	return r.plan
}

func TestResetPlanCoordinatesHub(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		hostState bool
		fail      string
		want      []string
	}{
		{name: "native worker reset", hostState: true, want: []string{"stop", "hub", "destroy"}},
		{name: "nspawn only", want: []string{"stop", "destroy"}},
		{name: "stop fails", hostState: true, fail: "stop", want: []string{"stop"}},
		{name: "hub fails", hostState: true, fail: "hub", want: []string{"stop", "hub"}},
		{name: "destroy fails", hostState: true, fail: "destroy", want: []string{"stop", "hub", "destroy"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var events []string
			task := func(name string) phases.Task {
				return hostnode.Func(name, func(context.Context) error {
					events = append(events, name)
					if tc.fail == name {
						return errors.New(name + " failed")
					}
					return nil
				})
			}
			plan := testResetPlan{hostState: tc.hostState, stop: task("stop"), finish: task("destroy")}
			err := ResetWorker(slog.New(slog.DiscardHandler), testResetRuntime{plan: plan}, task("hub")).Do(t.Context())
			if (err != nil) != (tc.fail != "") {
				t.Fatalf("reset error = %v", err)
			}
			if !slices.Equal(events, tc.want) {
				t.Fatalf("events = %v, want %v", events, tc.want)
			}
		})
	}
}
