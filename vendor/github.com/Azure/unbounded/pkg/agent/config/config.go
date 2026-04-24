// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package config defines the intermediate representation (IR) for agent
// configuration. These types are the shared contract between the agent
// library and its consumers.
//
// Consumers should define their own public-facing configuration
// representation and translate to/from these IR types. For example, AKS
// Flex Node maintains its own JSON config schema and maps it to these
// structs before calling agent library functions.
//
// TODO: the versioning and stability guarantees for this package are not
// yet finalized and will be revisited in a future iteration.
package config

// AgentConfig is the core configuration needed to bootstrap a Kubernetes
// node. It contains only the cloud-agnostic fields required by the shared
// agent library. Platform-specific extensions (e.g. attestation, cloud
// provider identity) should be defined in the consuming application.
type AgentConfig struct {
	MachineName string             `json:"MachineName"`
	Cluster     AgentClusterConfig `json:"Cluster"`
	Kubelet     AgentKubeletConfig `json:"Kubelet"`

	// OCIImage is the fully-qualified OCI image reference (e.g.
	// "ghcr.io/org/repo:tag") used to bootstrap the machine rootfs.
	// When empty the agent falls back to debootstrap.
	OCIImage string `json:"OCIImage,omitempty"`
}

// AgentClusterConfig holds the cluster-level values the agent needs to
// join the Kubernetes control plane.
type AgentClusterConfig struct {
	CaCertBase64 string `json:"CaCertBase64"`
	ClusterDNS   string `json:"ClusterDNS"`
	Version      string `json:"Version"`
}

// AgentKubeletConfig holds kubelet-specific overrides.
type AgentKubeletConfig struct {
	ApiServer          string            `json:"ApiServer"`
	BootstrapToken     string            `json:"BootstrapToken,omitempty"`
	Labels             map[string]string `json:"Labels"`
	RegisterWithTaints []string          `json:"RegisterWithTaints"`
}
