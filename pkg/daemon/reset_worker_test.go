package daemon

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/hostnode"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type testResetPlan struct {
	native bool
	stop   phases.Task
	finish phases.Task
}

func (p testResetPlan) HasNone() bool       { return p.native }
func (p testResetPlan) Stop() phases.Task   { return p.stop }
func (p testResetPlan) Finish() phases.Task { return p.finish }

func TestResetPlanCoordinatesHub(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		native bool
		fail   string
		want   []string
	}{
		{name: "native", native: true, want: []string{"stop", "hub", "destroy"}},
		{name: "nspawn only", want: []string{"stop", "destroy"}},
		{name: "stop fails", native: true, fail: "stop", want: []string{"stop"}},
		{name: "hub fails", native: true, fail: "hub", want: []string{"stop", "hub"}},
		{name: "destroy fails", native: true, fail: "destroy", want: []string{"stop", "hub", "destroy"}},
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
			plan := testResetPlan{native: tc.native, stop: task("stop"), finish: task("destroy")}
			err := runResetPlan(t.Context(), slog.New(slog.DiscardHandler), plan, task("hub"))
			if (err != nil) != (tc.fail != "") {
				t.Fatalf("reset error = %v", err)
			}
			if !slices.Equal(events, tc.want) {
				t.Fatalf("events = %v, want %v", events, tc.want)
			}
		})
	}
}
