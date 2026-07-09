package aksmachine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/client-go/rest"

	"github.com/Azure/AKSFlexNode/pkg/config"
)

func TestClusterEndpointClientGet(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/api/v1/namespaces/kube-system/services/http:aks-flex-controller:80/proxy/machines/node1"; got != want {
			http.Error(w, fmt.Sprintf("path = %q, want %q", got, want), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
  "id": "machine-id",
  "name": "node1",
  "properties": {
    "settings": {
      "kubernetesVersion": "1.34.0",
      "settingsVersion": "42",
      "maxPods": 42,
      "nodeLabels": {"workload": "flex"},
      "nodeTaints": ["dedicated=flex:NoSchedule"],
      "kubeletConfig": {"imageGCHighThreshold": 85, "imageGCLowThreshold": 80}
    },
    "status": {
      "provisioningState": "Succeeded",
      "observedSettingsVersion": "42",
      "message": "ok"
    }
  }
}`)
	}))
	defer server.Close()

	client := newTestClusterEndpointClient(t, server.URL, "node1")
	machine, err := client.Get(context.Background())
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if machine.ID != "machine-id" || machine.Name != "node1" {
		t.Fatalf("machine identity = %#v", machine)
	}
	if machine.Goal.KubernetesVersion != "1.34.0" || machine.Goal.SettingsVersion != "42" {
		t.Fatalf("goal = %#v", machine.Goal)
	}
	if machine.Goal.MaxPods != 42 || machine.Goal.NodeLabels["workload"] != "flex" || len(machine.Goal.NodeTaints) != 1 {
		t.Fatalf("extended goal = %#v", machine.Goal)
	}
	if machine.Status.ProvisioningState != ProvisioningStateSucceeded || machine.Status.ObservedSettingsVersion != "42" {
		t.Fatalf("status = %#v", machine.Status)
	}
}

func TestClusterEndpointClientNotFound(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	client := newTestClusterEndpointClient(t, server.URL, "node1")
	_, err := client.Get(context.Background())
	var notFound *NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("Get() error = %v, want NotFoundError", err)
	}
}

func TestClusterEndpointCreateVerifiesPrecreatedMachine(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"properties":{"settings":{"kubernetesVersion":"1.34.0","settingsVersion":"42"}}}`)
	}))
	defer server.Close()

	client := newTestClusterEndpointClient(t, server.URL, "node1")
	if _, err := client.Create(context.Background(), GoalState{KubernetesVersion: "1.34.0", SettingsVersion: "42"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	_, err := client.Create(context.Background(), GoalState{KubernetesVersion: "1.35.0", SettingsVersion: "42"})
	if err == nil || !strings.Contains(err.Error(), "Kubernetes version") {
		t.Fatalf("Create() error = %v, want version mismatch", err)
	}
}

func newTestClusterEndpointClient(t *testing.T, host, nodeName string) *clusterEndpointClient {
	t.Helper()
	cfg := &config.Config{
		Agent: config.AgentConfig{
			NodeName: nodeName,
			MachineClient: config.MachineClientConfig{
				Mode:        config.MachineClientModeInCluster,
				EndpointURL: "/api/v1/namespaces/kube-system/services/http:aks-flex-controller:80/proxy",
			},
		},
	}
	client, err := newClusterEndpointClient(cfg, slog.Default(), &rest.Config{Host: host})
	if err != nil {
		t.Fatalf("newClusterEndpointClient() error = %v", err)
	}
	return client
}
