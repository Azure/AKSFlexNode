package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/daemon/internal/provider"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestNoneStateRoundTrip(t *testing.T) {
	t.Parallel()
	store, err := newFileStateStore(filepath.Join(t.TempDir(), "nested", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	ac := &agentconfig.AgentConfig{Isolation: "none"}
	ac.Cluster.Version = "1.36.2"
	state := &State{AppliedSettingsVersion: "42", AppliedKubernetesVersion: "1.36.2"}
	provider.SeedNoneState(state, ac)
	for _, settings := range []string{"43", "44", "44"} {
		state.AppliedSettingsVersion = settings
		if err := store.Save(t.Context(), state); err != nil {
			t.Fatal(err)
		}
		got, err := store.Load(t.Context())
		if err != nil || !reflect.DeepEqual(got, state) {
			t.Fatalf("Load did not preserve none state: %v", err)
		}
	}
}

func TestNspawnStateFormatUnchanged(t *testing.T) {
	t.Parallel()
	for _, mode := range []isolation.Mode{"", isolation.ModeNSpawn} {
		t.Run(fmt.Sprintf("isolation=%q", mode), func(t *testing.T) {
			t.Parallel()
			store, err := newFileStateStore(filepath.Join(t.TempDir(), "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			state := &State{AppliedSettingsVersion: "42", ActiveMachine: "kube1", Isolation: mode}
			if err := store.Save(t.Context(), state); err != nil {
				t.Fatal(err)
			}
			want, err := json.MarshalIndent(state, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			want = append(want, '\n')
			data, err := os.ReadFile(store.path)
			if err != nil || !bytes.Equal(data, want) {
				t.Fatalf("nspawn state format changed: %v", err)
			}
			digest, err := os.ReadFile(store.checksumPath())
			if err != nil || string(digest) != checksum(want)+"\n" {
				t.Fatalf("nspawn checksum sidecar changed: %v", err)
			}
			got, err := store.Load(t.Context())
			expected := *state
			expected.Isolation = isolation.ModeNSpawn
			if err != nil || !reflect.DeepEqual(got, &expected) {
				t.Fatalf("nspawn Load did not preserve state: %v", err)
			}
		})
	}
}
