package daemon

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/internal/runtimeprovider"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestContainerStateRecord(t *testing.T) {
	t.Parallel()
	store, err := newFileStateStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	state := containerTestState("kube2", "42")
	for range 2 {
		if err := store.Save(t.Context(), state); err != nil {
			t.Fatal(err)
		}
		got, err := store.Load(t.Context())
		if err != nil || !reflect.DeepEqual(got, state) {
			t.Fatalf("Load = %+v, %v", got, err)
		}
	}
}

func TestContainerStartupRecord(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		mutate  func(*containerStartup)
		wantErr bool
	}{
		{name: "preparing"},
		{name: "starting", mutate: func(s *containerStartup) { s.Phase = "starting" }},
		{name: "finished", mutate: func(s *containerStartup) { s.Phase = "finished" }},
		{name: "unknown phase", mutate: func(s *containerStartup) { s.Phase = "unsafe" }, wantErr: true},
		{name: "same slot", mutate: func(s *containerStartup) { s.Previous = s.Next }, wantErr: true},
		{name: "wrong config slot", mutate: func(s *containerStartup) { s.Next.NodeConfig.MachineName = "kube1" }, wantErr: true},
		{name: "wrong mode", mutate: func(s *containerStartup) { s.Next.Isolation = isolation.ModeNSpawn }, wantErr: true},
		{name: "missing opt in", mutate: func(s *containerStartup) { s.Next.NodeConfig.Experimental = nil }, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := containerStartupStore{Path: filepath.Join(t.TempDir(), "startup.json")}
			startup := &containerStartup{Phase: "preparing", Next: containerTestState("kube2", "new"), Previous: containerTestState("kube1", "old")}
			if tc.mutate != nil {
				tc.mutate(startup)
			}
			for range 2 {
				if err := store.Save(t.Context(), startup); err != nil {
					t.Fatal(err)
				}
			}
			info, err := os.Stat(store.Path)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("startup permissions: %v", err)
			}
			got, err := store.Load()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Load = %+v, %v", got, err)
			}
			if !tc.wantErr && !reflect.DeepEqual(got, startup) {
				t.Fatal("startup round trip lost frozen fields")
			}
			for range 2 {
				if err := store.Delete(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestContainerAgentUpgradeSignal(t *testing.T) {
	t.Parallel()
	state := containerTestState("kube2", "42")
	store := agentUpgradeSignalStore{path: filepath.Join(t.TempDir(), "upgrade.json")}
	if err := recordPendingAgentUpgrade(context.Background(), store, &fakeNodeOperator{state: state}, "upgrade", "daemon", containerTestProvider(t, activationFenceJournal(t, "absent"))); err != nil {
		t.Fatal(err)
	}
	signal, err := store.read()
	if err != nil {
		t.Fatal(err)
	}
	if signal.Isolation != isolation.ModeContainer || signal.ActiveMachine != "kube2" {
		t.Fatalf("wrong signal: %+v", signal)
	}
	if strings.Contains(runtimeprovider.ContainerAgentBinaryPath(signal.ActiveMachine), "/var/lib/machines/") {
		t.Fatal("container signal selects nspawn synchronization")
	}
}
