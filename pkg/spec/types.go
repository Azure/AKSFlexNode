package spec

import "time"

const (
	// ManagedClusterSpecSchemaVersion is incremented when the persisted JSON schema changes.
	ManagedClusterSpecSchemaVersion = 1
)

// ManagedClusterSpec is the persisted spec snapshot of the target AKS managed cluster.
// It is intentionally extensible so we can add more fields over time without rewriting the collector.
type ManagedClusterSpec struct {
	SchemaVersion int `json:"schemaVersion"`

	ClusterResourceID string `json:"clusterResourceId,omitempty"`
	ClusterName       string `json:"clusterName,omitempty"`
	ResourceGroup     string `json:"resourceGroup,omitempty"`

	// KubernetesVersion is kept as a first-class field because many components care about it.
	KubernetesVersion        string `json:"kubernetesVersion,omitempty"`        // "e.g., 1.32"
	CurrentKubernetesVersion string `json:"currentKubernetesVersion,omitempty"` // "e.g., 1.32.7"
	Fqdn                     string `json:"fqdn,omitempty"`

	// metadata
	CollectedAt time.Time `json:"collectedAt"`
}
