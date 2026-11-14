package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
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
	if _, err := os.Stat(store.checksumPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("container state created a legacy sidecar: %v", err)
	}
	data, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	var record noneStateRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record.Checksum != checksum(record.Payload) {
		t.Fatal("state publication is not checksummed")
	}
	data = bytes.Replace(data, []byte(`"42"`), []byte(`"43"`), 1)
	if err := os.WriteFile(store.path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(t.Context()); err == nil {
		t.Fatal("tampered container state was accepted")
	}
}

func TestContainerStartupRecord(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name             string
		mutate           func(*containerStartup)
		corrupt, wantErr bool
	}{
		{name: "preparing"},
		{name: "starting", mutate: func(s *containerStartup) { s.Phase = "starting" }},
		{name: "finished", mutate: func(s *containerStartup) { s.Phase = "finished" }},
		{name: "unknown phase", mutate: func(s *containerStartup) { s.Phase = "unsafe" }, wantErr: true},
		{name: "same slot", mutate: func(s *containerStartup) { s.Previous = s.Next }, wantErr: true},
		{name: "missing registration", mutate: func(s *containerStartup) { delete(s.Next.NodeConfig.Kubelet.Labels, noneGenerationLabel) }, wantErr: true},
		{name: "wrong config slot", mutate: func(s *containerStartup) { s.Next.NodeConfig.MachineName = "kube1" }, wantErr: true},
		{name: "wrong mode", mutate: func(s *containerStartup) { s.Next.Isolation = isolation.ModeNSpawn }, wantErr: true},
		{name: "missing opt in", mutate: func(s *containerStartup) { s.Next.NodeConfig.Experimental = nil }, wantErr: true},
		{name: "tampered payload", corrupt: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := containerStartupStore{path: filepath.Join(t.TempDir(), "startup.json")}
			startup := &containerStartup{Phase: "preparing", Next: containerTestState("kube2", "new"), Previous: containerTestState("kube1", "old")}
			if tc.mutate != nil {
				tc.mutate(startup)
			}
			for range 2 {
				if err := store.Save(t.Context(), startup); err != nil {
					t.Fatal(err)
				}
			}
			info, err := os.Stat(store.path)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("startup permissions: %v", err)
			}
			if tc.corrupt {
				data, err := os.ReadFile(store.path)
				if err != nil {
					t.Fatal(err)
				}
				data = bytes.Replace(data, []byte(`"new"`), []byte(`"bad"`), 1)
				if err := os.WriteFile(store.path, data, 0o600); err != nil {
					t.Fatal(err)
				}
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

func TestContainerAgentUpgradeRoutingAndCapability(t *testing.T) {
	t.Parallel()
	for _, mode := range []isolation.Mode{isolation.ModeNSpawn, isolation.ModeContainer} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			want := "/var/lib/machines/kube2/usr/local/bin/aks-flex-node"
			if mode == isolation.ModeContainer {
				want = filepath.Join(container.Layout{}.RootfsDir("kube2"), "usr/local/bin/aks-flex-node")
			}
			if got := activeNodeAgentBinaryPath(mode, "kube2"); got != want {
				t.Fatalf("binary sync path = %s, want %s", got, want)
			}
		})
	}
	if err := synchronizeNodeAgentBinary("missing", isolation.ModeNone, ""); err != nil {
		t.Fatal(err)
	}
	if err := synchronizeNodeAgentBinary("missing", isolation.ModeContainer, "../escape"); err == nil {
		t.Fatal("container sync accepted invalid slot")
	}
	for _, tc := range []struct {
		name, supported string
		wantErr         bool
	}{
		{name: "container capable", supported: "container"},
		{name: "none only refused", supported: "none", wantErr: true},
		{name: "old agent refused", supported: "nspawn", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			candidate := filepath.Join(t.TempDir(), "agent")
			script := "#!/bin/sh\n[ \"$1\" = supports-isolation ] && [ \"$2\" = " + tc.supported + " ]\n"
			if err := os.WriteFile(candidate, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := checkAgentIsolation(t.Context(), candidate, isolation.ModeContainer); (err != nil) != tc.wantErr {
				t.Fatalf("capability = %v", err)
			}
		})
	}
}

func TestContainerAgentUpgradeSignal(t *testing.T) {
	t.Parallel()
	state := containerTestState("kube2", "42")
	store := agentUpgradeSignalStore{path: filepath.Join(t.TempDir(), "upgrade.json")}
	if err := recordPendingAgentUpgrade(context.Background(), store, &fakeNodeOperator{state: state}, "upgrade", "daemon"); err != nil {
		t.Fatal(err)
	}
	signal, err := store.read()
	if err != nil {
		t.Fatal(err)
	}
	if signal.Isolation != isolation.ModeContainer || signal.ActiveMachine != "kube2" {
		t.Fatalf("wrong signal: %+v", signal)
	}
	if strings.Contains(activeNodeAgentBinaryPath(signal.Isolation, signal.ActiveMachine), "/var/lib/machines/") {
		t.Fatal("container signal selects nspawn synchronization")
	}
}
