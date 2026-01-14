package status

import (
	"time"
)

// NodeStatus represents the current status and health information of the AKS edge node
type NodeStatus struct {
	// Component versions
	KubeletVersion string `json:"kubeletVersion"`
	RuncVersion    string `json:"runcVersion"`

	// Service status
	KubeletRunning bool `json:"kubeletRunning"`

	// Azure Arc status
	ArcStatus ArcStatus `json:"arcStatus"`

	// Metadata
	LastUpdated  time.Time `json:"lastUpdated"`
	AgentVersion string    `json:"agentVersion"`
}

// ArcStatus contains Azure Arc machine registration and connection status
type ArcStatus struct {
	Registered    bool      `json:"registered"`
	Connected     bool      `json:"connected"`
	MachineName   string    `json:"machineName"`
	ResourceID    string    `json:"resourceId,omitempty"`
	Location      string    `json:"location,omitempty"`
	ResourceGroup string    `json:"resourceGroup,omitempty"`
	LastHeartbeat time.Time `json:"lastHeartbeat,omitempty"`
	AgentVersion  string    `json:"agentVersion,omitempty"`
}
