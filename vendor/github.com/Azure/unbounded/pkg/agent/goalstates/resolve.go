// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/Azure/unbounded/pkg/agent/config"
)

// MachineGoalState holds the fully resolved goal state for provisioning and
// starting an nspawn machine. Callers use RootFS for the rootfs provisioning
// phases and NodeStart for the service configuration and boot phases.
type MachineGoalState struct {
	RootFS    *RootFS
	NodeStart *NodeStart
}

// ResolveMachine probes the host (kernel version, hostname, GPU hardware) and
// resolves the complete goal state for the named nspawn machine from an agent
// config.
func ResolveMachine(log *slog.Logger, cfg *config.AgentConfig, machineName string) (*MachineGoalState, error) {
	kernel, err := hostKernel()
	if err != nil {
		return nil, fmt.Errorf("get host kernel: %w", err)
	}

	hostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("get host hostname: %w", err)
	}

	nvidia, err := ResolveNvidiaHost(runtime.GOARCH)
	if err != nil {
		return nil, fmt.Errorf("resolve nvidia host: %w", err)
	}

	ociImage := ResolveOCIImage(log, cfg.OCIImage, len(nvidia.GPUDevicePaths) > 0)

	kubelet, err := resolveKubelet(cfg)
	if err != nil {
		return nil, fmt.Errorf("resolve kubelet config: %w", err)
	}

	rootFS := &RootFS{
		MachineDir: filepath.Join("/var/lib/machines", machineName),
		NSpawnConfigFile: filepath.Join(
			SystemdNSpawnDir,
			machineName+".nspawn",
		),
		ServiceOverrideFile: filepath.Join(
			SystemdSystemDir,
			fmt.Sprintf("systemd-nspawn@%s.service.d", machineName),
			"override.conf",
		),
		HostArch:          runtime.GOARCH,
		HostKernel:        kernel,
		Hostname:          hostname,
		ContainerdVersion: ContainerdVersion,
		RunCVersion:       RunCVersion,
		CNIPluginVersion:  CNIPluginVersion,
		KubernetesVersion: cfg.Cluster.Version,
		OCIImage:          ociImage,
		Nvidia:            nvidia,
		HostDevicePaths:   DiscoverHostDevicePaths(),
	}

	nodeStart := &NodeStart{
		MachineName:     machineName,
		KubeMachineName: cfg.MachineName,
		MachineDir:      filepath.Join("/var/lib/machines", machineName),
		Containerd:      ResolveContainerd(),
		Kubelet:         kubelet,
		Nvidia:          nvidia,
	}

	return &MachineGoalState{
		RootFS:    rootFS,
		NodeStart: nodeStart,
	}, nil
}

// resolveKubelet builds the kubelet goal state from an agent config.
func resolveKubelet(cfg *config.AgentConfig) (Kubelet, error) {
	var zero Kubelet

	caCert, err := base64.StdEncoding.DecodeString(cfg.Cluster.CaCertBase64)
	if err != nil {
		return zero, fmt.Errorf("decode CaCertBase64: %w", err)
	}

	labels := cfg.Kubelet.Labels
	if labels == nil {
		labels = make(map[string]string)
	}

	if err := cfg.Kubelet.Auth.Validate(); err != nil {
		return zero, fmt.Errorf("kubelet auth: %w", err)
	}

	return Kubelet{
		KubeletBinPath:     filepath.Join("/"+BinDir, "kubelet"),
		KubeletAuthInfo:    cfg.Kubelet.Auth,
		APIServer:          cfg.Kubelet.ApiServer,
		CACertData:         caCert,
		ClusterDNS:         cfg.Cluster.ClusterDNS,
		NodeLabels:         labels,
		RegisterWithTaints: cfg.Kubelet.RegisterWithTaints,
	}, nil
}

// ResolveOCIImage determines the OCI image to use for the nspawn rootfs.
//
// Priority (highest to lowest):
//  1. configImage from the agent config
//  2. AGENT_DISABLE_OCI_IMAGE env var (truthy value disables OCI, returns "")
//  3. AGENT_OCI_IMAGE env var
//  4. Built-in default selected by GPU presence
func ResolveOCIImage(log *slog.Logger, configImage string, nvidiaGPUAvailable bool) string {
	if configImage != "" {
		return configImage
	}

	if disabled, err := strconv.ParseBool(os.Getenv("AGENT_DISABLE_OCI_IMAGE")); err == nil && disabled {
		log.Info("OCI image usage disabled via AGENT_DISABLE_OCI_IMAGE, falling back to debootstrap")
		return ""
	}

	if v := strings.TrimSpace(os.Getenv("AGENT_OCI_IMAGE")); v != "" {
		return v
	}

	var image string
	if nvidiaGPUAvailable {
		image = DefaultNvidiaOCImage
	} else {
		image = DefaultOCIImage
	}

	log.Info("no OCI image configured, using default", "image", image)

	return image
}
