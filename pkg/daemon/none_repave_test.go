package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/internal/agentruntime"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type noneRepaveRuntime struct {
	fakeWorkerRuntime
	root   string
	events *[]string
}

func (r *noneRepaveRuntime) Worker(_ *slog.Logger, _ agentruntime.Node) (agentruntime.WorkerContext, error) {
	return agentruntime.WorkerContext{
		Root: isolation.DirRoot{Dir: r.root},
		Exec: func(_ context.Context, args ...string) (string, error) {
			*r.events = append(*r.events, "hub-cleanup")
			return "", nil
		},
		KubeletUnit: none.UnitKubelet,
		Mode:        isolation.ModeNone,
	}, nil
}

func (r *noneRepaveRuntime) Prepare(_ *slog.Logger, input agentruntime.Inputs) phases.Task {
	return testTask{
		name: "recover",
		run: func() error {
			*r.events = append(*r.events, "recover")
			if input.Previous.Config.Cluster.Version != "1.34.0" {
				return errors.New("wrong applied config")
			}
			return nil
		},
	}
}

func (r *noneRepaveRuntime) Start(_ *slog.Logger, input agentruntime.Inputs) phases.Task {
	return testTask{
		name: "upgrade",
		run: func() error {
			*r.events = append(*r.events, "upgrade")
			if input.Node.Slot != "kube1" || input.Node.Config.Cluster.Version != "1.35.0" {
				return errors.New("wrong target config")
			}
			if _, err := os.Stat(filepath.Join(r.root, "var/lib/aks-flex-node/hub-transition.json")); !errors.Is(err, os.ErrNotExist) {
				return errors.New("hub journal still exists")
			}
			return nil
		},
	}
}

func (r *noneRepaveRuntime) Retire(_ *slog.Logger, _ agentruntime.Inputs) phases.Task {
	return testTask{
		name: "commit",
		run: func() error {
			*r.events = append(*r.events, "commit")
			return nil
		},
	}
}

func TestNoneRepaveSavesBeforeCommitAndCleansHub(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		failSave bool
		want     []string
	}{
		{
			name: "success",
			want: []string{"recover", "hub-cleanup", "upgrade", "save", "commit"},
		},
		{
			name:     "save fails",
			failSave: true,
			want:     []string{"recover", "hub-cleanup", "upgrade", "save"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			journal := filepath.Join(root, "var/lib/aks-flex-node/hub-transition.json")
			if err := os.MkdirAll(filepath.Dir(journal), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(journal, []byte("journal"), 0o600); err != nil {
				t.Fatal(err)
			}
			var events []string
			worker := &noneRepaveRuntime{root: root, events: &events}
			previous := &agentconfig.AgentConfig{
				Isolation: "none",
				Cluster:   agentconfig.AgentClusterConfig{Version: "1.34.0"},
			}
			store := &recordingStateStore{
				state: &State{
					Isolation:     isolation.ModeNone,
					ActiveMachine: "kube1",
					NodeConfig:    previous,
				},
				events: &events,
				fail:   tc.failSave,
			}
			r := newTestRepaveReconciler(t, &fakeMachineClient{}, fakeClient(), &fakeWorkerRuntime{})
			r.runtime, r.state = worker, store
			r.cfg.Bootstrap.Isolation = isolation.ModeNone
			r.resolve = func(context.Context, *slog.Logger, *config.Config, string) (*agentconfig.AgentConfig, *goalstates.MachineGoalState, *goalstates.ContainerImageArchiveStaging, error) {
				return &agentconfig.AgentConfig{
					Isolation: "none",
					Cluster:   agentconfig.AgentClusterConfig{Version: "1.35.0"},
				}, &goalstates.MachineGoalState{}, nil, nil
			}
			_, err := r.executeGoalState(t.Context(), aksmachine.GoalState{SettingsVersion: "new", KubernetesVersion: "1.35.0"})
			if (err != nil) != tc.failSave {
				t.Fatalf("execute error = %v", err)
			}
			if !reflect.DeepEqual(events, tc.want) {
				t.Fatalf("events = %v, want %v", events, tc.want)
			}
			if !tc.failSave && (store.state.NodeConfig.Cluster.Version != "1.35.0" || store.state.ActiveMachine != "kube1") {
				t.Fatal("target SDK config was not persisted")
			}
		})
	}
}
