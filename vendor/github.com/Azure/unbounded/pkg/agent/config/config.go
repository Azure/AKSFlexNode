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

import (
	"fmt"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

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
	Auth               KubeletAuthInfo   `json:"Auth"`
	Labels             map[string]string `json:"Labels"`
	RegisterWithTaints []string          `json:"RegisterWithTaints"`
}

// KubeletAuthInfo holds the kubelet authentication configuration.
// Exactly one of BootstrapToken or ExecCredential must be set.
type KubeletAuthInfo struct {
	// BootstrapToken is a Kubernetes bootstrap token in
	// "<token-id>.<token-secret>" format used for TLS bootstrapping.
	BootstrapToken string `json:"BootstrapToken,omitempty"`

	// ExecCredential configures kubelet to authenticate via a
	// client.authentication.k8s.io exec credential plugin.
	ExecCredential *clientcmdapi.ExecConfig `json:"ExecCredential,omitempty"`
}

// Validate checks that exactly one auth method is configured.
func (a *KubeletAuthInfo) Validate() error {
	hasToken := a.BootstrapToken != ""
	hasExec := a.ExecCredential != nil
	switch {
	case hasToken && hasExec:
		return fmt.Errorf("BootstrapToken and ExecCredential are mutually exclusive")
	case !hasToken && !hasExec:
		return fmt.Errorf("one of BootstrapToken or ExecCredential must be set")
	}
	if hasExec && a.ExecCredential.Command == "" {
		return fmt.Errorf("ExecCredential.Command is required")
	}
	return nil
}
