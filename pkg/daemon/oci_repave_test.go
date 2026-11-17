package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/internal/agentruntime"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type ociRepaveRuntime struct {
	fakeWorkerRuntime
	events *[]string
}

func (r *ociRepaveRuntime) PrepareUpgrade(_ *slog.Logger, input agentruntime.Inputs) phases.Task {
	return testTask{
		name: "prepare",
		run: func() error {
			*r.events = append(*r.events, "prepare")
			if input.Previous.Slot != "kube1" || input.Node.Slot != "kube2" {
				return fmt.Errorf("incorrect OCI slots")
			}
			return nil
		},
	}
}

func (r *ociRepaveRuntime) Start(_ *slog.Logger, input agentruntime.Inputs) phases.Task {
	return testTask{
		name: "upgrade",
		run: func() error {
			*r.events = append(*r.events, "upgrade")
			if input.Previous.Config.Cluster.Version != "1.34.0" || input.Node.Config.Cluster.Version != "1.35.0" {
				return fmt.Errorf("incorrect OCI configs")
			}
			return nil
		},
	}
}

func (r *ociRepaveRuntime) Retire(_ *slog.Logger, _ agentruntime.Inputs) phases.Task {
	return testTask{
		name: "retire",
		run: func() error {
			*r.events = append(*r.events, "retire")
			return nil
		},
	}
}

func TestOCIRepaveSavesAlternateSlotBeforeRetire(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		failSave bool
		want     []string
	}{
		{
			name: "success",
			want: []string{"prepare", "upgrade", "save", "retire"},
		},
		{
			name:     "save fails",
			failSave: true,
			want:     []string{"prepare", "upgrade", "save"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var events []string
			store := &recordingStateStore{
				state: &State{
					Isolation:     isolation.ModeContainer,
					ActiveMachine: "kube1",
					NodeConfig: &agentconfig.AgentConfig{
						Isolation: "container",
						Cluster: agentconfig.AgentClusterConfig{
							Version: "1.34.0",
						},
					},
				},
				events: &events,
				fail:   tc.failSave,
			}
			r := newTestRepaveReconciler(t, &fakeMachineClient{}, fakeClient(), &fakeWorkerRuntime{})
			r.runtime = &ociRepaveRuntime{events: &events}
			r.state = store
			r.cfg.Bootstrap.Isolation = isolation.ModeContainer
			r.resolve = func(_ context.Context, _ *slog.Logger, _ *config.Config, _ string) (*agentconfig.AgentConfig, *goalstates.MachineGoalState, *goalstates.ContainerImageArchiveStaging, error) {
				return &agentconfig.AgentConfig{
					Isolation: "container",
					Cluster: agentconfig.AgentClusterConfig{
						Version: "1.35.0",
					},
				}, &goalstates.MachineGoalState{}, nil, nil
			}
			_, err := r.executeGoalState(t.Context(), aksmachine.GoalState{
				SettingsVersion:   "new",
				KubernetesVersion: "1.35.0",
			})
			if (err != nil) != tc.failSave || !reflect.DeepEqual(events, tc.want) {
				t.Fatalf("error=%v events=%v, want %v", err, events, tc.want)
			}
			if !tc.failSave && (store.state.ActiveMachine != "kube2" || store.state.NodeConfig.Cluster.Version != "1.35.0") {
				t.Fatal("target slot and configuration were not persisted")
			}
		})
	}
}
