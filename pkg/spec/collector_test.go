package spec

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v5"
	"github.com/sirupsen/logrus"

	"go.goms.io/aks/AKSFlexNode/pkg/config"
)

type fakeManagedClusterClient struct {
	resp armcontainerservice.ManagedClustersClientGetResponse
	err  error
}

func (f *fakeManagedClusterClient) Get(ctx context.Context, resourceGroupName, resourceName string, options *armcontainerservice.ManagedClustersClientGetOptions) (armcontainerservice.ManagedClustersClientGetResponse, error) {
	return f.resp, f.err
}

func ptr[T any](v T) *T { return &v }

func TestManagedClusterSpecCollector_Collect_WritesFile(t *testing.T) {
	cfg := &config.Config{
		Azure: config.AzureConfig{
			SubscriptionID: "sub",
			TargetCluster: &config.TargetClusterConfig{
				Name:          "c1",
				ResourceGroup: "rg1",
				ResourceID:    "/subscriptions/sub/resourceGroups/rg1/providers/Microsoft.ContainerService/managedClusters/c1",
			},
		},
	}

	outDir := t.TempDir()
	outPath := filepath.Join(outDir, "managedcluster.json")

	resp := armcontainerservice.ManagedClustersClientGetResponse{
		ManagedCluster: armcontainerservice.ManagedCluster{
			Properties: &armcontainerservice.ManagedClusterProperties{
				KubernetesVersion:        ptr("1.30.1"),
				CurrentKubernetesVersion: ptr("1.30.9"),
				Fqdn:                     ptr("c1-12345.hcp.eastus.azmk8s.io"),
			},
		},
	}

	collector := NewManagedClusterSpecCollectorWithClient(cfg, logrus.New(), &fakeManagedClusterClient{resp: resp}, outPath)
	_, err := collector.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	// #nosec G304 -- test reads a temp file path created by the test harness.
	b, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	var got ManagedClusterSpec
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if got.KubernetesVersion != "1.30.1" {
		t.Fatalf("expected version 1.30.1, got %q", got.KubernetesVersion)
	}
	if got.CurrentKubernetesVersion != "1.30.9" {
		t.Fatalf("expected current version 1.30.9, got %q", got.CurrentKubernetesVersion)
	}
	if got.Fqdn != "c1-12345.hcp.eastus.azmk8s.io" {
		t.Fatalf("expected fqdn %q, got %q", "c1-12345.hcp.eastus.azmk8s.io", got.Fqdn)
	}
	if got.SchemaVersion != ManagedClusterSpecSchemaVersion {
		t.Fatalf("expected schemaVersion %d, got %d", ManagedClusterSpecSchemaVersion, got.SchemaVersion)
	}
	if got.ClusterName != "c1" || got.ResourceGroup != "rg1" {
		t.Fatalf("unexpected cluster metadata: %+v", got)
	}
}

func TestManagedClusterSpecCollector_Collect_MissingClusterInfo(t *testing.T) {
	cfg := &config.Config{Azure: config.AzureConfig{SubscriptionID: "sub", TargetCluster: &config.TargetClusterConfig{}}}
	collector := NewManagedClusterSpecCollectorWithClient(cfg, logrus.New(), &fakeManagedClusterClient{}, filepath.Join(t.TempDir(), "x.json"))
	if _, err := collector.Collect(context.Background()); err == nil {
		t.Fatalf("expected error, got nil")
	}
}
