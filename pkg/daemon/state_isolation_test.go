package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestAppliedNoneStateRoundTrip(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	store, err := newFileStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	want := &State{
		Isolation:     isolation.ModeNone,
		ActiveMachine: "kube1",
		NodeConfig: &agentconfig.AgentConfig{
			Isolation: string(isolation.ModeNone),
			Kubelet: agentconfig.AgentKubeletConfig{
				KubeconfigData: []byte("credential"),
				Labels:         map[string]string{"role": "worker"},
			},
		},
	}
	for _, name := range []string{path, path + ".sha256"} {
		if err := os.WriteFile(name, []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(name, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Save(t.Context(), want); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(t.Context())
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("state mismatch: %v", err)
	}
	for _, name := range []string{path, path + ".sha256"} {
		info, err := os.Stat(name)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("state permissions for %s: %v", name, err)
		}
	}
	original, err := json.Marshal(want.NodeConfig)
	if err != nil {
		t.Fatal(err)
	}
	copyJSON, err := json.Marshal(got.NodeConfig)
	if err != nil || string(copyJSON) != string(original) {
		t.Fatalf("SDK configuration did not retain its recovery digest: %v", err)
	}
}

func TestAppliedStateModeValidation(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		state   *State
		want    isolation.Mode
		wantErr bool
	}{
		{
			name:  "legacy",
			state: &State{},
			want:  isolation.ModeNSpawn,
		},
		{
			name: "missing none config",
			state: &State{
				Isolation:     isolation.ModeNone,
				ActiveMachine: "kube1",
			},
			wantErr: true,
		},
		{
			name: "mismatched none config",
			state: &State{
				Isolation:     isolation.ModeNone,
				ActiveMachine: "kube1",
				NodeConfig:    &agentconfig.AgentConfig{Isolation: "nspawn"},
			},
			wantErr: true,
		},
		{
			name: "invalid none slot",
			state: &State{
				Isolation:     isolation.ModeNone,
				ActiveMachine: "kube2",
				NodeConfig:    &agentconfig.AgentConfig{Isolation: "none"},
			},
			wantErr: true,
		},
		{
			name:    "unknown",
			state:   &State{Isolation: "other"},
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store, err := newFileStateStore(filepath.Join(t.TempDir(), "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Save(t.Context(), tc.state); err != nil {
				t.Fatal(err)
			}
			got, err := store.Load(t.Context())
			if (err != nil) != tc.wantErr {
				t.Fatalf("load error = %v", err)
			}
			if err == nil && got.Isolation != tc.want {
				t.Fatalf("mode = %q, want %q", got.Isolation, tc.want)
			}
		})
	}
}
