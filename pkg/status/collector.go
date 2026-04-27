package status

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/kube"
	"github.com/Azure/AKSFlexNode/pkg/spec"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Collector collects system and node status information.
// After the nspawn migration, kubelet and containerd run inside the nspawn
// machine rather than directly on the host.
type Collector struct {
	config       *config.Config
	logger       *slog.Logger
	agentVersion string
	machineName  string
}

// NewCollector creates a new status collector. machineName is the nspawn
// machine name where kubelet/containerd are running (e.g. "kube1").
func NewCollector(cfg *config.Config, logger *slog.Logger, agentVersion string, machineName string) *Collector {
	return &Collector{
		config:       cfg,
		logger:       logger,
		agentVersion: agentVersion,
		machineName:  machineName,
	}
}

// CollectStatus collects essential node status information
func (c *Collector) CollectStatus(ctx context.Context) (*NodeStatus, error) {
	status := &NodeStatus{
		LastUpdated:  time.Now(),
		AgentVersion: c.agentVersion,
	}

	// Get kubelet related status (running inside nspawn machine)
	status.KubeletVersion = c.getKubeletVersion(ctx)
	status.KubeletRunning = c.isServiceActiveInMachine(ctx, "kubelet")
	status.KubeletReady = c.isKubeletReady(ctx)

	// get containerd related status (running inside nspawn machine)
	status.ContainerdVersion = c.getContainerdVersion(ctx)
	status.ContainerdRunning = c.isServiceActiveInMachine(ctx, "containerd")

	// Get runc version (inside nspawn machine)
	status.RuncVersion = c.getRuncVersion(ctx)

	// Collect Arc status (runs on host, not inside nspawn)
	arcStatus, err := c.collectArcStatus(ctx)
	if err != nil {
		c.logger.Warn("failed to collect Arc status", "error", err)
	}
	status.ArcStatus = arcStatus

	return status, nil
}

// machineRun executes a command inside the nspawn machine and returns its stdout.
func (c *Collector) machineRun(ctx context.Context, args ...string) (string, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return utilexec.MachineRun(timeoutCtx, c.logger, c.machineName, args...)
}

// isServiceActiveInMachine checks if a systemd service is active inside the nspawn machine.
func (c *Collector) isServiceActiveInMachine(ctx context.Context, serviceName string) bool {
	output, err := c.machineRun(ctx, "systemctl", "is-active", serviceName)
	if err != nil {
		return false
	}
	return strings.TrimSpace(output) == "active"
}

// getKubeletVersion gets the kubelet version from inside the nspawn machine
func (c *Collector) getKubeletVersion(ctx context.Context) string {
	output, err := c.machineRun(ctx, "/usr/local/bin/kubelet", "--version")
	if err != nil {
		c.logger.Warn("failed to get kubelet version", "error", err)
		return "unknown"
	}

	// Extract version from output like "Kubernetes v1.32.7"
	parts := strings.Fields(strings.TrimSpace(output))
	if len(parts) >= 2 {
		return strings.TrimPrefix(parts[1], "v")
	}

	c.logger.Warn("failed to parse kubelet version", "output", output)
	return "unknown"
}

func (c *Collector) getContainerdVersion(ctx context.Context) string {
	output, err := c.machineRun(ctx, "containerd", "--version")
	if err != nil {
		c.logger.Warn("failed to get containerd version", "error", err)
		return "unknown"
	}

	// Extract version from output like "containerd github.com/containerd/containerd v1.7.20 8fc6bcff..."
	parts := strings.Fields(strings.TrimSpace(output))
	if len(parts) >= 3 {
		return strings.TrimPrefix(parts[2], "v")
	}
	return "unknown"
}

// getRuncVersion gets the runc version from inside the nspawn machine
func (c *Collector) getRuncVersion(ctx context.Context) string {
	output, err := c.machineRun(ctx, "runc", "--version")
	if err != nil {
		c.logger.Warn("failed to get runc version", "error", err)
		return "unknown"
	}

	// Parse runc version output
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for _, line := range lines {
		if strings.Contains(line, "version") {
			parts := strings.Fields(line)
			for i, part := range parts {
				if part == "version" && i+1 < len(parts) {
					return parts[i+1]
				}
			}
		}
	}

	c.logger.Warn("failed to parse runc version", "output", output)
	return "unknown"
}

// collectArcStatus gathers Azure Arc machine registration and connection status
// Arc runs on the host, not inside the nspawn machine.
func (c *Collector) collectArcStatus(ctx context.Context) (ArcStatus, error) {
	status := ArcStatus{}

	// Try to get comprehensive Arc status from azcmagent show (runs on host)
	timeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	output, err := utilexec.OutputCmd(timeoutCtx, c.logger, "azcmagent", "show")
	if err == nil {
		c.parseArcShowOutput(&status, output)
	} else {
		c.logger.Debug("azcmagent show failed, marking Arc as disconnected", "error", err)
		status.Connected = false
		status.Registered = false
	}

	return status, nil
}

// parseArcShowOutput parses the output of 'azcmagent show' and populates ArcStatus
func (c *Collector) parseArcShowOutput(status *ArcStatus, output string) {
	lines := strings.Split(strings.TrimSpace(output), "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, ":") {
			continue
		}

		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		switch key {
		case "Agent Status":
			status.Connected = strings.ToLower(value) == "connected"
			status.Registered = status.Connected
		case "Agent Last Heartbeat":
			if heartbeat, err := time.Parse("2006-01-02T15:04:05Z", value); err == nil {
				status.LastHeartbeat = heartbeat
			}
		case "Resource Name":
			if status.MachineName == "" {
				status.MachineName = value
			}
		case "Resource Group Name":
			if status.ResourceGroup == "" {
				status.ResourceGroup = value
			}
		case "Location":
			if status.Location == "" {
				status.Location = value
			}
		case "Resource Id":
			status.ResourceID = value
		}
	}
}

// isKubeletReady checks if the kubelet reports the node as Ready
func (c *Collector) isKubeletReady(ctx context.Context) string {
	hostName, err := os.Hostname()
	if err != nil {
		c.logger.Warn("failed to get hostname", "error", err)
		return "Unknown"
	}

	cs, err := kube.KubeletClientset()
	if err != nil {
		c.logger.Warn("failed to create kubelet clientset for readiness", "error", err)
		return "Unknown"
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	n, err := cs.CoreV1().Nodes().Get(timeoutCtx, hostName, metav1.GetOptions{})
	if err != nil {
		c.logger.Warn("failed to get node for readiness", "node", hostName, "error", err)
		return "Unknown"
	}

	for _, cond := range n.Status.Conditions {
		if cond.Type != corev1.NodeReady {
			continue
		}
		switch cond.Status {
		case corev1.ConditionTrue:
			return "Ready"
		case corev1.ConditionFalse:
			return "NotReady"
		default:
			return "Unknown"
		}
	}
	return "Unknown"
}

// NeedsBootstrap checks if the node needs to be (re)bootstrapped based on status file
func (c *Collector) NeedsBootstrap(ctx context.Context) bool {
	statusFilePath := spec.StatusFilePath
	// Try to read the status file
	// #nosec G304 -- reading a local status snapshot path controlled by the agent, not user input.
	statusData, err := os.ReadFile(statusFilePath)
	if err != nil {
		c.logger.Info("status file not found, bootstrap needed")
		return true
	}

	var nodeStatus NodeStatus
	if err := json.Unmarshal(statusData, &nodeStatus); err != nil {
		c.logger.Info("could not parse status file, bootstrap needed")
		return true
	}

	// Check if status indicates unhealthy conditions
	if !nodeStatus.KubeletRunning {
		c.logger.Info("status file indicates kubelet not running, bootstrap needed")
		return true
	}

	// Check if Arc status is unhealthy (if configured)
	if c.config != nil && c.config.IsARCEnabled() && c.config.GetArcMachineName() != "" {
		if !nodeStatus.ArcStatus.Connected {
			c.logger.Info("status file indicates Arc agent not connected, bootstrap needed")
			return true
		}
	}

	// Check if status is too old (older than 5 minutes might indicate daemon issues)
	if time.Since(nodeStatus.LastUpdated) > 5*time.Minute {
		c.logger.Info("status file is stale (older than 5 minutes), bootstrap needed")
		return true
	}

	// Check for essential component versions being unknown (indicates collection failures)
	if nodeStatus.KubeletVersion == "unknown" || nodeStatus.KubeletVersion == "" {
		c.logger.Info("status file indicates kubelet version unknown, bootstrap needed")
		return true
	}

	if nodeStatus.RuncVersion == "unknown" || nodeStatus.RuncVersion == "" {
		c.logger.Info("status file indicates runc version unknown, bootstrap needed")
		return true
	}

	c.logger.Debug("status file indicates healthy state, no bootstrap needed")
	return false
}
