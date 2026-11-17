package runtimeprovider_test

import (
	"log/slog"
	"slices"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/daemon"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestNodeProviderFactory(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name            string
		mode            isolation.Mode
		want            isolation.Mode
		nspawn, wantErr bool
	}{
		{name: "empty mode", wantErr: true},
		{name: "nspawn", mode: isolation.ModeNSpawn, want: isolation.ModeNSpawn, nspawn: true},
		{name: "none", mode: isolation.ModeNone, want: isolation.ModeNone},
		{name: "container", mode: isolation.ModeContainer, want: isolation.ModeContainer},
		{name: "unknown", mode: "invalid", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, err := daemon.NewNodeProvider(tc.mode)
			if (err != nil) != tc.wantErr {
				t.Fatalf("factory=%v, %v", p, err)
			}
			if tc.wantErr {
				return
			}
			if p.Mode() != tc.want || p.SupportsDisconnectTolerance() != tc.nspawn || (p.NSpawnRuntime() != nil) != tc.nspawn {
				t.Fatalf("wrong provider capabilities: %T", p)
			}
			if err := p.ValidateEnvironment(false); err != nil {
				t.Fatal(err)
			}
			if err := p.ValidateEnvironment(true); (err == nil) != tc.nspawn {
				t.Fatalf("containerized environment=%v", err)
			}
			wanted := []string{"network-online.target", "systemd-udev-settle.service"}
			if tc.nspawn {
				wanted = append(wanted, "machines.target")
			}
			if !slices.Equal(p.WantedTargets(), wanted) {
				t.Fatalf("targets=%v", p.WantedTargets())
			}
			if got := len(p.RuntimeChecks(&config.Config{}, slog.New(slog.DiscardHandler), false)); (got > 0) != tc.nspawn {
				t.Fatalf("runtime checks=%d", got)
			}
		})
	}
}
