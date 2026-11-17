package provider

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/containerops"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostnode/hostnodetest"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
)

func TestContainerSDKPrepareInstallsFlexArtifactsAfterUnpack(t *testing.T) {
	t.Parallel()
	for _, fail := range []bool{false, true} {
		name := "successful SDK unpack"
		if fail {
			name = "SDK unpack failed"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			layout := container.Layout{Base: t.TempDir()}
			log := slog.New(slog.DiscardHandler)
			called := false
			unpackErr := errors.New("unpack failed")
			node := containerops.New(containerops.Params{Log: log, Slot: "kube2", GoalState: hostnodetest.GoalState("kube2", false), Overrides: containerops.Overrides{Layout: layout, Unpack: func(ctx context.Context, dir string) error {
				called = true
				if fail {
					return unpackErr
				}
				return os.MkdirAll(dir, 0o750)
			}}})
			cfg := &config.Config{Bootstrap: config.BootstrapConfig{Isolation: isolation.ModeContainer, OfflineArtifacts: config.OfflineArtifactsConfig{Source: "test-offline-source"}}}
			gs, err := bindContainerArtifacts(node, log, cfg)
			if err != nil {
				t.Fatal(err)
			}
			dir := layout.RootfsDir("kube2")
			err = node.Backend().Unpack(t.Context(), isolation.NodeSpec{Slot: "kube2"}, dir)
			if !called || (err != nil) != fail {
				t.Fatalf("unpack called=%t, err=%v", called, err)
			}
			helper := gs.RootFS.Root().Path("/usr/local/bin/aks-flex-node")
			if fail {
				if !errors.Is(err, unpackErr) {
					t.Fatalf("lost SDK error: %v", err)
				}
				if _, err := os.Stat(helper); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("installed helper before successful SDK unpack: %v", err)
				}
			} else {
				if helper != filepath.Join(dir, "usr/local/bin/aks-flex-node") {
					t.Fatal("incorrect worker helper path")
				}
				if info, err := os.Stat(helper); err != nil || info.Mode().Perm() != 0o750 {
					t.Fatalf("worker helper not installed before SDK starts kubelet: %v", err)
				}
			}
		})
	}
}

func TestContainerHelperUsesStableHostAlias(t *testing.T) {
	t.Parallel()
	node := containerNode(slog.New(slog.DiscardHandler), Node{Slot: "kube1", GoalState: &goalstates.MachineGoalState{RootFS: &goalstates.RootFS{}, NodeStart: &goalstates.NodeStart{}}})
	if node.Backend().HelperPath != "/opt/unbounded/agent/bin/aks-flex-node" {
		t.Fatalf("helper=%s", node.Backend().HelperPath)
	}
}
