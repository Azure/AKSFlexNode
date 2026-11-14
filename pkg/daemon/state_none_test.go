package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestNoneStateRoundTrip(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "nested", "state.json")
			store, err := newFileStateStore(path)
			if err != nil {
				t.Fatal(err)
			}
			ac := &agentconfig.AgentConfig{Isolation: "none"}
			ac.Cluster.Version = "1.36.2"
			state := &State{AppliedSettingsVersion: "42", AppliedKubernetesVersion: "1.36.2"}
			seedNoneState(state, ac, "v1.35.1")

			var legacyChecksum []byte
			if legacy {
				data, err := json.MarshalIndent(state, "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				data = append(data, '\n')
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
				legacyChecksum = []byte(checksum(data) + "\n")
				if err := os.WriteFile(store.checksumPath(), legacyChecksum, 0o600); err != nil {
					t.Fatal(err)
				}
				got, err := store.Load(t.Context())
				if err != nil || !reflect.DeepEqual(got, state) {
					t.Fatalf("legacy Load did not preserve state: %v", err)
				}
			}

			for _, settings := range []string{"43", "44", "44"} {
				state.AppliedSettingsVersion = settings
				if err := store.Save(t.Context(), state); err != nil {
					t.Fatal(err)
				}
				got, err := store.Load(t.Context())
				if err != nil || !reflect.DeepEqual(got, state) {
					t.Fatalf("Load did not preserve none state: %v", err)
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var record noneStateRecord
				if err := json.Unmarshal(data, &record); err != nil {
					t.Fatal(err)
				}
				if record.Version != noneStateRecordVersion || record.Checksum != checksum(record.Payload) {
					t.Fatal("Save did not publish a checksummed versioned record")
				}
			}

			data, err := os.ReadFile(store.checksumPath())
			if legacy {
				if err != nil || !bytes.Equal(data, legacyChecksum) {
					t.Fatalf("none Save changed the obsolete sidecar: %v", err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("none Save created a checksum sidecar: %v", err)
			}
		})
	}
}

func TestNoneStatePermissions(t *testing.T) {
	t.Parallel()
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%t", existing), func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "state.json")
			store, err := newFileStateStore(path)
			if err != nil {
				t.Fatal(err)
			}
			if existing {
				if err := os.WriteFile(path, []byte("old state"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // verifies publication tightens an existing readable file
					t.Fatal(err)
				}
			}
			if err := store.Save(t.Context(), &State{Isolation: isolation.ModeNone}); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("applied config permissions = %o, want 600", info.Mode().Perm())
			}
		})
	}
}

func TestNoneStateRawPayloadChecksum(t *testing.T) {
	t.Parallel()
	payload := []byte("{\n  \"appliedSettingsVersion\": \"42\",\n  \"isolation\": \"none\"\n}")
	store, err := newFileStateStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	data := fmt.Appendf(nil, `{"version":1,"payload":%s,"checksum":%q}`, payload, checksum(payload))
	if err := os.WriteFile(store.path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got.Isolation != isolation.ModeNone || got.AppliedSettingsVersion != "42" {
		t.Fatal("Load did not preserve the raw payload")
	}
}

func TestNoneStateChecksumCorruption(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		changePayload bool
		changeSpace   bool
	}{
		{name: "checksum"},
		{name: "payload", changePayload: true},
		{name: "payload whitespace", changeSpace: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, err := newFileStateStore(filepath.Join(t.TempDir(), "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			payload := []byte(`{"isolation": "none", "appliedSettingsVersion": "42"}`)
			digest := checksum(payload)
			switch {
			case tc.changePayload:
				payload = bytes.Replace(payload, []byte(`"42"`), []byte(`"43"`), 1)
			case tc.changeSpace:
				payload = bytes.Replace(payload, []byte(": "), []byte(":"), 1)
			default:
				digest = strings.Repeat("0", len(digest))
			}
			data := fmt.Appendf(nil, `{"version":1,"payload":%s,"checksum":%q}`, payload, digest)
			if err := os.WriteFile(store.path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			// A matching legacy sidecar must not bypass record validation.
			if err := os.WriteFile(store.checksumPath(), []byte(checksum(data)+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := store.Load(t.Context())
			if err == nil || !strings.Contains(err.Error(), "checksum mismatch") || got != nil {
				t.Fatalf("Load error = %v, want checksum mismatch and no state", err)
			}
		})
	}
}

func TestNoneStateRecordVersion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, field string
	}{
		{name: "missing"},
		{name: "zero", field: `"version":0,`},
		{name: "unsupported", field: `"version":2,`},
		{name: "negative", field: `"version":-1,`},
		{name: "null", field: `"version":null,`},
		{name: "string", field: `"version":"1",`},
		{name: "boolean", field: `"version":true,`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, err := newFileStateStore(filepath.Join(t.TempDir(), "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			payload := []byte(`{"isolation":"none"}`)
			data := fmt.Appendf(nil, `{%s"payload":%s,"checksum":%q}`, tc.field, payload, checksum(payload))
			if err := os.WriteFile(store.path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(store.checksumPath(), []byte(checksum(data)+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := store.Load(t.Context())
			if err == nil || !strings.Contains(err.Error(), "version") || got != nil {
				t.Fatalf("Load error = %v, want record version error and no state", err)
			}
		})
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
			if err != nil || !reflect.DeepEqual(got, state) {
				t.Fatalf("nspawn Load did not preserve state: %v", err)
			}
		})
	}
}
