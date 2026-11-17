package daemon

import (
	"log/slog"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestOCIUpgradeHelperUsesActiveSlot(t *testing.T) {
	t.Parallel()

	helpers, err := agentUpgradeHelperPaths(slog.New(slog.DiscardHandler), isolation.ModeContainer)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		slot string
		want string
	}{
		{
			slot: "kube1",
			want: "/var/lib/unbounded/containers/kube1/usr/local/bin/aks-flex-node",
		},
		{
			slot: "kube2",
			want: "/var/lib/unbounded/containers/kube2/usr/local/bin/aks-flex-node",
		},
	} {
		t.Run(tc.slot, func(t *testing.T) {
			t.Parallel()
			if got := helpers[tc.slot]; got != tc.want {
				t.Fatalf("credential helper = %q, want %q", got, tc.want)
			}
		})
	}
}
