package nodebootstrap

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

const validPartialConfig = `{
  "azure": {
    "bootstrapToken": {"token": "abcdef.0123456789abcdef"},
    "arc": {"enabled": false},
    "targetCluster": {
      "resourceId": "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
      "location": "eastus"
    }
  },
  "components": {"kubernetes": "1.35.0"},
  "node": {
    "kubelet": {
      "clusterFQDN": "cluster.example.test:443",
      "caCertData": "Y2E="
    }
  }
}`

func TestRenderConfig(t *testing.T) {
	t.Parallel()

	data, cfg, err := RenderConfig(context.Background(), []byte(validPartialConfig), ConfigRenderOptions{
		Hostname: "NODE-01",
		GOOS:     "linux",
		GOARCH:   "arm64",
		StringArgs: []string{
			"nodeIP=10.0.0.4",
		},
		JSONArgs: []string{
			"maxPods=250",
		},
		Queries: []string{
			`.node.kubelet.nodeIP = $nodeIP | .node.maxPods = $maxPods | .node.labels.arch = $arch`,
		},
	})
	if err != nil {
		t.Fatalf("RenderConfig() error = %v", err)
	}
	if cfg.Agent.NodeName != "node-01" {
		t.Fatalf("Agent.NodeName = %q, want node-01", cfg.Agent.NodeName)
	}
	if cfg.Node.Kubelet.NodeIP != "10.0.0.4" {
		t.Fatalf("Node.Kubelet.NodeIP = %q, want 10.0.0.4", cfg.Node.Kubelet.NodeIP)
	}
	if cfg.Node.MaxPods != 250 {
		t.Fatalf("Node.MaxPods = %d, want 250", cfg.Node.MaxPods)
	}
	if cfg.Node.Labels["arch"] != "arm64" {
		t.Fatalf("Node.Labels[arch] = %q, want arm64", cfg.Node.Labels["arch"])
	}

	var rendered struct {
		Azure struct {
			TargetCluster map[string]any `json:"targetCluster"`
		} `json:"azure"`
	}
	if err := json.Unmarshal(data, &rendered); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	for _, field := range []string{"Name", "ResourceGroup", "SubscriptionID"} {
		if _, ok := rendered.Azure.TargetCluster[field]; ok {
			t.Fatalf("rendered targetCluster contains runtime-only field %q", field)
		}
	}
}

func TestRenderConfigDefaultsArcMachineName(t *testing.T) {
	t.Parallel()

	partial := strings.Replace(
		validPartialConfig,
		`"azure": {`,
		`"azure": {"tenantId": "12345678-1234-1234-1234-123456789012", "subscriptionId": "12345678-1234-1234-1234-123456789012",`,
		1,
	)
	partial = strings.Replace(
		partial,
		`"arc": {"enabled": false}`,
		`"arc": {"enabled": true, "resourceGroup": "arc-rg", "location": "eastus"}`,
		1,
	)
	_, cfg, err := RenderConfig(context.Background(), []byte(partial), ConfigRenderOptions{Hostname: "EDGE-NODE-01"})
	if err != nil {
		t.Fatalf("RenderConfig() error = %v", err)
	}
	if cfg.Azure.Arc == nil || cfg.Azure.Arc.MachineName != "edge-node-01" {
		t.Fatalf("Arc.MachineName = %v, want edge-node-01", cfg.Azure.Arc)
	}
}

func TestRenderConfigQueryErrors(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		options ConfigRenderOptions
		want    string
	}{
		"argument without query": {
			options: ConfigRenderOptions{StringArgs: []string{"value=test"}},
			want:    "require at least one --jq",
		},
		"reserved argument": {
			options: ConfigRenderOptions{Queries: []string{"."}, StringArgs: []string{"nodeName=test"}},
			want:    "duplicated or reserved",
		},
		"invalid JSON argument": {
			options: ConfigRenderOptions{Queries: []string{"."}, JSONArgs: []string{"value={"}},
			want:    "parse --jq-argjson",
		},
		"no result": {
			options: ConfigRenderOptions{Queries: []string{"empty"}},
			want:    "returned no result",
		},
		"multiple results": {
			options: ConfigRenderOptions{Queries: []string{"., ."}},
			want:    "returned multiple results",
		},
		"non-object result": {
			options: ConfigRenderOptions{Queries: []string{"42"}},
			want:    "must return a JSON object",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, _, err := RenderConfig(context.Background(), []byte(validPartialConfig), ConfigRenderOptions{
				Hostname:   "node-01",
				Queries:    test.options.Queries,
				StringArgs: test.options.StringArgs,
				JSONArgs:   test.options.JSONArgs,
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("RenderConfig() error = %v, want containing %q", err, test.want)
			}
		})
	}
}
