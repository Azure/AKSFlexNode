package runtimeprovider

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

func TestNoneBaseSoftRestartDoesNotRepave(t *testing.T) {
	t.Parallel()
	f := newCompletionFixture(t)
	if err := f.op.recover(t.Context(), discardLogger()); err != nil {
		t.Fatal(err)
	}
	credentials := filepath.Join(f.params.HostRoot, goalstates.KubeletKubeconfigPath)
	if err := os.MkdirAll(filepath.Dir(credentials), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentials, []byte("client credentials"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls, cleanups := len(f.calls), f.cleanups
	for range 2 {
		if err := f.op.RestartNode(t.Context(), discardLogger()); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		"systemctl restart " + none.UnitContainerd,
		"systemctl restart " + none.UnitKubelet,
		"systemctl restart " + none.UnitContainerd,
		"systemctl restart " + none.UnitKubelet,
	}
	if !slices.Equal(f.calls[calls:], want) || f.cleanups != cleanups {
		t.Fatalf("soft restart changed pod/network lifecycle: %v", f.calls[calls:])
	}
	if data, err := os.ReadFile(credentials); err != nil || string(data) != "client credentials" {
		t.Fatalf("soft restart replaced credentials: %v", err)
	}
	if f.store.state.AppliedSettingsVersion != "41" {
		t.Fatal("soft restart changed applied settings")
	}
	f.assertCommitted(t)
	f.assertAppliedRuntime(t, f.store.state)
}
