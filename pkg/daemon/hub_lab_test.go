//go:build hub_lab

package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/kubeauth"
	"github.com/Azure/AKSFlexNode/pkg/yurthub"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/nspawn"
)

// TestHubLab drives the production Hub reconciler and operations on a disposable,
// already-joined Flex host, with RP intent supplied by a JSON fixture instead of
// a Machine read. It is excluded from release builds.
//
// Build: go test -tags hub_lab -c -o hub-lab.test ./pkg/daemon
// Run as root with the host daemon stopped (systemctl stop aks-flex-node-agent) so
// real Machine reads don't race the fixture:
//
//	FLEX_HUB_LAB_CONFIG=intent.json ./hub-lab.test -test.run '^TestHubLab$' -test.v
//
// intent.json: {"enabled": true, "activeMachine": "<worker>"}. Cluster prerequisites
// are in docs/design/yurthub-agent-integration.md. Don't delete the worker's
// hub-transition.json journal to get past a failed restore.
func TestHubLab(t *testing.T) {
	path := os.Getenv("FLEX_HUB_LAB_CONFIG")
	if path == "" {
		t.Skip("set FLEX_HUB_LAB_CONFIG on a disposable Flex host")
	}
	var input struct {
		Enabled       *bool  `json:"enabled"`
		ActiveMachine string `json:"activeMachine"`
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &input); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig("/etc/aks-flex-node/config.json")
	if err != nil {
		t.Fatal(err)
	}
	restCfg, err := kubeauth.BootstrapRESTConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Use the real host daemon certificate, never an admin kubeconfig or the
	// worker kubelet identity. Do not add Node-write RBAC to make this pass.
	restCfg.BearerToken = ""
	restCfg.ExecProvider = nil
	restCfg.CertFile = "/etc/aks-flex-node/daemon-credentials/daemon-controller-current.pem"
	restCfg.KeyFile = restCfg.CertFile
	nodeCfg := rest.CopyConfig(restCfg)
	nodeCfg.Timeout = 30 * time.Second
	client, err := kubernetes.NewForConfig(nodeCfg)
	if err != nil {
		t.Fatal(err)
	}
	hub := yurthub.NewReconciler(slog.Default(), cfg.Agent.NodeName, cfg.Azure.TargetAgentPoolName, cfg.APIServerURL(), client.CoreV1().Nodes(), time.Duration(cfg.Agent.MachineReconcileInterval))
	var worker yurthub.Worker
	if validActiveMachine(input.ActiveMachine) {
		slot := input.ActiveMachine
		worker = yurthub.Worker{
			Scope: slot,
			Root:  nspawn.Root(slot),
			Exec: func(ctx context.Context, args ...string) (string, error) {
				return nspawn.Exec(ctx, slog.Default(), slot, args...)
			},
			KubeletUnit: "kubelet.service",
			Mode:        isolation.ModeNSpawn,
		}
	}
	if err := hub.Reconcile(t.Context(), input.Enabled, worker, nil); err != nil {
		t.Fatal(err)
	}
}
