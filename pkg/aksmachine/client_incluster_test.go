package aksmachine

import (
	"context"
	"errors"
	"fmt"
	"io"
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
    "eTag": "42",
    "kubernetes": {
      "orchestratorVersion": "1.34.0",
      "maxPods": 42,
      "nodeLabels": {"workload": "flex"},
      "nodeTaints": ["dedicated=flex:NoSchedule"],
      "kubeletConfig": {"imageGcHighThreshold": 85, "imageGcLowThreshold": 80}
    },
    "provisioningState": "Succeeded"
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
	if machine.Status.ProvisioningState != ProvisioningStateSucceeded {
		t.Fatalf("status = %#v", machine.Status)
	}
}

func TestMachineFromEndpointJSONUsesARMModel(t *testing.T) {
	t.Parallel()

	machine, err := machineFromEndpointJSON([]byte(`{
  "id": "machine-id",
  "name": "node1",
  "properties": {
    "eTag": "42",
    "kubernetes": {"orchestratorVersion": "1.34.0"},
    "provisioningState": "Succeeded"
  }
}`))
	if err != nil {
		t.Fatalf("machineFromEndpointJSON() error = %v", err)
	}
	if machine.ID != "machine-id" || machine.Name != "node1" {
		t.Fatalf("machine identity = %#v", machine)
	}
	if machine.Goal.KubernetesVersion != "1.34.0" || machine.Goal.SettingsVersion != "42" {
		t.Fatalf("goal = %#v", machine.Goal)
	}
	if machine.Status.ProvisioningState != ProvisioningStateSucceeded {
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

func TestClusterEndpointCreateSendsMutation(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, fmt.Sprintf("method = %q, want PUT", r.Method), http.StatusInternalServerError)
			return
		}
		if got, want := r.URL.Path, "/api/v1/namespaces/kube-system/services/http:aks-flex-controller:80/proxy/machines/node1"; got != want {
			http.Error(w, fmt.Sprintf("path = %q, want %q", got, want), http.StatusInternalServerError)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !strings.Contains(string(body), `"orchestratorVersion":"1.34.0"`) {
			http.Error(w, fmt.Sprintf("body = %s, want desired version", body), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"properties":{"eTag":"42","kubernetes":{"orchestratorVersion":"1.34.0"}}}`)
	}))
	defer server.Close()

	client := newTestClusterEndpointClient(t, server.URL, "node1")
	if _, err := client.Create(context.Background(), GoalState{KubernetesVersion: "1.34.0", SettingsVersion: "42"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
}

func TestClusterEndpointCreateVerifiesPrecreatedMachine(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"properties":{"eTag":"42","kubernetes":{"orchestratorVersion":"1.34.0"}}}`)
	}))
	defer server.Close()

	client := newTestClusterEndpointClient(t, server.URL, "node1")
	_, err := client.Create(context.Background(), GoalState{KubernetesVersion: "1.35.0", SettingsVersion: "42"})
	if err == nil || !strings.Contains(err.Error(), "Kubernetes version") {
		t.Fatalf("Create() error = %v, want version mismatch", err)
	}
}

func TestClusterEndpointPatchStatusSendsMutation(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			http.Error(w, fmt.Sprintf("method = %q, want PATCH", r.Method), http.StatusInternalServerError)
			return
		}
		if got, want := r.URL.Path, "/api/v1/namespaces/kube-system/services/http:aks-flex-controller:80/proxy/machines/node1/status"; got != want {
			http.Error(w, fmt.Sprintf("path = %q, want %q", got, want), http.StatusInternalServerError)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !strings.Contains(string(body), `"provisioningState":"Succeeded"`) {
			http.Error(w, fmt.Sprintf("body = %s, want status", body), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := newTestClusterEndpointClient(t, server.URL, "node1")
	if err := client.PatchStatus(context.Background(), Status{ProvisioningState: ProvisioningStateSucceeded}); err != nil {
		t.Fatalf("PatchStatus() error = %v", err)
	}
}

func TestNewMachineClientRoutesToInClusterClient(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Agent: config.AgentConfig{
			NodeName: "node1",
			MachineClient: config.MachineClientConfig{
				Mode:        config.MachineClientModeInCluster,
				EndpointURL: "/api/v1/namespaces/kube-system/services/http:aks-flex-controller:80/proxy",
			},
		},
	}
	client, err := NewMachineClient(cfg, slog.Default(), MachineClientOptions{
		KubernetesRESTConfig: &rest.Config{Host: "https://cluster.example"},
	})
	if err != nil {
		t.Fatalf("NewMachineClient() error = %v", err)
	}
	if _, ok := client.(*clusterEndpointClient); !ok {
		t.Fatalf("NewMachineClient() type = %T, want *clusterEndpointClient", client)
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
