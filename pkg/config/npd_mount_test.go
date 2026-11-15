package config

import (
	"reflect"
	"testing"

	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestNPDKernelLogMount(t *testing.T) {
	t.Parallel()

	kernelMount := agentconfig.AdditionalHostMount{Source: "/dev/kmsg", ReadOnly: true}
	for _, tc := range []struct {
		name    string
		mode    isolation.Mode
		offline string
		mounts  []AdditionalHostMount
		want    []agentconfig.AdditionalHostMount
	}{
		{name: "default nspawn", want: []agentconfig.AdditionalHostMount{kernelMount}},
		{name: "explicit nspawn", mode: isolation.ModeNSpawn, want: []agentconfig.AdditionalHostMount{kernelMount}},
		{name: "none already uses host device", mode: isolation.ModeNone},
		{name: "container supplies its device", mode: isolation.ModeContainer},
		{name: "offline NPD disabled", offline: "file:///opt/artifacts"},
		{name: "whitespace is not offline", offline: " ", want: []agentconfig.AdditionalHostMount{kernelMount}},
		{
			name: "preserve caller mount", mounts: []AdditionalHostMount{{Source: "/opt/config", ReadOnly: true}},
			want: []agentconfig.AdditionalHostMount{{Source: "/opt/config", ReadOnly: true}, kernelMount},
		},
		{
			name: "existing same-path mount", mounts: []AdditionalHostMount{kernelMount},
			want: []agentconfig.AdditionalHostMount{kernelMount},
		},
		{
			name: "existing explicit target", mounts: []AdditionalHostMount{{Source: "/dev/kmsg", Target: "/dev/kmsg", ReadOnly: true}},
			want: []agentconfig.AdditionalHostMount{{Source: "/dev/kmsg", Target: "/dev/kmsg", ReadOnly: true}},
		},
		{
			name: "do not overwrite caller target", mounts: []AdditionalHostMount{{Source: "/dev/console", Target: "/dev/kmsg"}},
			want: []agentconfig.AdditionalHostMount{{Source: "/dev/console", Target: "/dev/kmsg"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &Config{Bootstrap: BootstrapConfig{
				Isolation: string(tc.mode), AdditionalHostMounts: tc.mounts,
				OfflineArtifacts: OfflineArtifactsConfig{Source: tc.offline},
			}}
			for range 2 {
				got := ToAgentConfig(cfg, "kube1").AdditionalHostMounts
				if !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("mounts = %#v, want %#v", got, tc.want)
				}
				if !reflect.DeepEqual(cfg.Bootstrap.AdditionalHostMounts, tc.mounts) {
					t.Fatalf("adaptation mutated caller mounts: %#v", cfg.Bootstrap.AdditionalHostMounts)
				}
			}
		})
	}
}
