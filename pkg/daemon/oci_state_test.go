package daemon

import (
	"path/filepath"
	"reflect"
	"testing"

	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestOCIStatePersistsAppliedSlotAndConfiguration(t *testing.T) {
	t.Parallel()
	store, err := newFileStateStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := &State{
		Isolation:     isolation.ModeContainer,
		ActiveMachine: "kube2",
		NodeConfig: &agentconfig.AgentConfig{
			Isolation: string(isolation.ModeContainer),
			Kubelet:   agentconfig.AgentKubeletConfig{KubeconfigData: []byte("credential")},
		},
	}
	if err := store.Save(t.Context(), want); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(t.Context())
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("state mismatch: %v", err)
	}
}

func TestOCIStateRequiresMatchingConfiguration(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		config *agentconfig.AgentConfig
	}{
		{name: "missing"},
		{name: "wrong mode", config: &agentconfig.AgentConfig{Isolation: string(isolation.ModeNone)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, err := newFileStateStore(filepath.Join(t.TempDir(), "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Save(t.Context(), &State{Isolation: isolation.ModeContainer, ActiveMachine: "kube2", NodeConfig: tc.config}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Load(t.Context()); err == nil {
				t.Fatal("accepted incomplete OCI state")
			}
		})
	}
}
