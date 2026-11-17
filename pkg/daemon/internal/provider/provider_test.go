package provider

import (
	"log/slog"
	"strings"
	"testing"

	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

func TestProviderLayoutAndSDKView(t *testing.T) {
	t.Parallel()
	for _, mode := range []isolation.Mode{isolation.ModeNSpawn, isolation.ModeNone, isolation.ModeContainer} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			p, err := New(mode)
			if err != nil {
				t.Fatal(err)
			}
			cfg := &agentconfig.AgentConfig{Isolation: string(mode)}
			gs := &goalstates.MachineGoalState{RootFS: &goalstates.RootFS{MachineDir: t.TempDir(), KubernetesVersion: "1.35.0", HostArch: "amd64"}, NodeStart: &goalstates.NodeStart{MachineName: "kube1", MachineDir: t.TempDir()}}
			got, err := p.GoalState(slog.New(slog.DiscardHandler), Node{Config: cfg, GoalState: gs, Slot: "kube1"})
			if err != nil {
				t.Fatal(err)
			}
			if got != gs {
				t.Fatal("SDK view replaced the resolved goal state")
			}
			layout := p.Layout()
			switch mode {
			case isolation.ModeNone:
				if layout.NextSlot("kube1") != "kube1" || layout.Directory("kube1") != "/" || layout.KubeletUnit != none.UnitKubelet || layout.CredentialHelperPath("kube1") != "" || len(layout.Targets) != 0 {
					t.Fatal("incorrect host-native layout")
				}
				if got.NodeStart.ExecOverride == nil || got.RootFS.Root().Path("/etc/test") != "/etc/test" || !strings.HasPrefix(got.NodeStart.Kubelet.KubeletBinPath, "/opt/unbounded/node/current/bin/") {
					t.Fatal("missing SDK none overrides")
				}
			case isolation.ModeContainer:
				if layout.Mode != mode || layout.NextSlot("kube1") != "kube2" || layout.Directory("kube1") != "/var/lib/unbounded/containers/kube1" || layout.CredentialHelperPath("kube1") != "/var/lib/unbounded/containers/kube1/usr/local/bin/aks-flex-node" {
					t.Fatal("incorrect OCI layout")
				}
				if got.NodeStart.ExecOverride == nil || !got.NodeStart.InContainer || got.RootFS.Root().Path("/var/lib/kubelet/kubeconfig") != "/var/lib/kubelet/kubeconfig" {
					t.Fatal("missing SDK container overrides")
				}
			case isolation.ModeNSpawn:
				if layout.NextSlot("kube1") != "kube2" || layout.Directory("kube1") != "/var/lib/machines/kube1" || layout.KubeletUnit != "kubelet.service" || layout.CredentialHelperPath("kube1") != "/var/lib/machines/kube1/usr/local/bin/aks-flex-node" || len(layout.Targets) != 1 || layout.Targets[0] != "machines.target" {
					t.Fatal("nspawn placement changed")
				}
				if got.NodeStart.ExecOverride != nil {
					t.Fatal("nspawn executor was overridden")
				}
			}
		})
	}
}

func TestProviderRejectsUnnormalizedModes(t *testing.T) {
	t.Parallel()
	for _, mode := range []isolation.Mode{"", "invalid"} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			if _, err := New(mode); err == nil {
				t.Fatal("unsupported mode accepted")
			}
		})
	}
}
