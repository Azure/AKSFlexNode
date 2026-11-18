package agentruntime

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/hostnode"
	"github.com/Azure/unbounded/pkg/agent/hostnode/hostnodetest"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/noneops"
)

func TestNoneWorkerUsesActiveVersionForHub(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	layout := none.Layout{Base: filepath.Join(root, "node")}
	if _, err := layout.Ensure("installed"); err != nil {
		t.Fatal(err)
	}
	if _, err := layout.Flip("installed"); err != nil {
		t.Fatal(err)
	}
	runtime := noneRuntime{newNode: func(params noneops.Params) *noneops.Node {
		params.GoalState = hostnodetest.GoalState("kube1", false)
		params.Overrides = noneops.Overrides{Layout: layout, Proc: t.TempDir()}
		params.Hooks = hostnode.Hooks{HostRoot: root}
		return noneops.New(params)
	}}
	view, err := runtime.Worker(slog.New(slog.DiscardHandler), Node{
		Slot:   "kube1",
		Config: &agentconfig.AgentConfig{Isolation: "none"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := view.Root.Path("/usr/local/bin/yurthub"), filepath.Join(root, "node", "installed", "bin", "yurthub"); got != want {
		t.Fatalf("hub executable path = %q, want %q", got, want)
	}
}

func TestNoneRestartStoppedServicesPreservesCredentials(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	credential := filepath.Join(root, "var/lib/kubelet/kubeconfig")
	if err := os.MkdirAll(filepath.Dir(credential), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credential, []byte("installed identity"), 0o600); err != nil {
		t.Fatal(err)
	}
	var commands []string
	runtime := noneRuntime{newNode: func(params noneops.Params) *noneops.Node {
		params.GoalState = hostnodetest.GoalState("kube1", false)
		params.Overrides = noneops.Overrides{
			Layout:      none.Layout{Base: filepath.Join(root, "node")},
			SystemdDir:  filepath.Join(root, "units"),
			Proc:        t.TempDir(),
			WaitKubelet: func(context.Context) error { return nil },
			Run: func(_ context.Context, _ *slog.Logger, command string, args ...string) (string, error) {
				commands = append(commands, append([]string{command}, args...)...)
				return "", nil
			},
		}
		params.Hooks = hostnode.Hooks{HostRoot: root}
		return noneops.New(params)
	}}
	applied := &agentconfig.AgentConfig{Isolation: string(isolation.ModeNone)}
	cfg := &config.Config{
		Bootstrap: config.BootstrapConfig{
			OfflineArtifacts: config.OfflineArtifactsConfig{Source: "offline"},
		},
	}
	input := Inputs{
		Config: cfg,
		Node: Node{
			Slot:   "kube1",
			Config: applied,
		},
	}
	if err := runtime.Restart(slog.New(slog.DiscardHandler), input).Do(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := []string{"systemctl", "restart", none.UnitContainerd, "systemctl", "restart", none.UnitKubelet}
	if !reflect.DeepEqual(commands, want) {
		t.Fatalf("restart commands = %v, want %v", commands, want)
	}
	got, err := os.ReadFile(credential)
	if err != nil || string(got) != "installed identity" {
		t.Fatalf("installed credentials changed: %v", err)
	}
}
