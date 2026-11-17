package daemon

import (
	"path/filepath"
	"reflect"
	"testing"

	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestStateDeepCopyIsPure(t *testing.T) {
	t.Parallel()
	original := &State{ActiveMachine: "kube1", NodeConfig: &agentconfig.AgentConfig{Kubelet: agentconfig.AgentKubeletConfig{Labels: map[string]string{"label": "original"}, Configuration: map[string]any{"logging": map[string]any{"verbosity": float64(2)}}}}}
	copy := original.DeepCopy()
	if !reflect.DeepEqual(original, copy) {
		t.Fatal("DeepCopy changed state intent or defaults")
	}
	copy.NodeConfig.Kubelet.Labels["label"] = "changed"
	copyLog, ok := copy.NodeConfig.Kubelet.Configuration["logging"].(map[string]any)
	if !ok {
		t.Fatal("copied logging configuration has wrong type")
	}
	copyLog["verbosity"] = float64(9)
	originalLog, ok := original.NodeConfig.Kubelet.Configuration["logging"].(map[string]any)
	if !ok {
		t.Fatal("original logging configuration has wrong type")
	}
	if original.Isolation != "" || original.NodeConfig.Kubelet.Labels["label"] != "original" || originalLog["verbosity"] != float64(2) {
		t.Fatal("copy shares runtime configuration or normalizes state")
	}
}

func TestLoadedIsolationIsNormalizedAndValidated(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		state State
		want  isolation.Mode
		valid bool
	}{
		{name: "legacy nspawn", state: State{ActiveMachine: "kube1"}, want: isolation.ModeNSpawn, valid: true},
		{name: "none", state: State{Isolation: isolation.ModeNone, ActiveMachine: "kube1", NodeConfig: &agentconfig.AgentConfig{Isolation: "none"}}, want: isolation.ModeNone, valid: true},
		{name: "missing applied config", state: State{Isolation: isolation.ModeNone}},
		{name: "unknown mode", state: State{Isolation: "other"}},
		{name: "mismatched config", state: State{Isolation: isolation.ModeNone, NodeConfig: &agentconfig.AgentConfig{Isolation: "nspawn"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, err := newFileStateStore(filepath.Join(t.TempDir(), stateFileName))
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Save(t.Context(), &tc.state); err != nil {
				t.Fatal(err)
			}
			got, err := store.Load(t.Context())
			if (err == nil) != tc.valid {
				t.Fatalf("load error=%v, valid=%t", err, tc.valid)
			}
			if tc.valid && got.Isolation != tc.want {
				t.Fatalf("stored isolation=%q, want %q", got.Isolation, tc.want)
			}
		})
	}
}
