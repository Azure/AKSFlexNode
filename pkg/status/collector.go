package status

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"go.goms.io/aks/AKSFlexNode/pkg/components/kubelet"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
)

// Collector collects system and node status information
type Collector struct {
	config       *config.Config
	logger       *logrus.Logger
	agentVersion string
}

// NewCollector creates a new status collector
func NewCollector(cfg *config.Config, logger *logrus.Logger, agentVersion string) *Collector {
	return &Collector{
		config:       cfg,
		logger:       logger,
		agentVersion: agentVersion,
	}
}

// CollectStatus collects essential node status information
func (c *Collector) CollectStatus(ctx context.Context) (*NodeStatus, error) {
	status := &NodeStatus{
		LastUpdated:  time.Now(),
		AgentVersion: c.agentVersion,
	}

	// Get kubelet related status
	status.KubeletVersion = c.getKubeletVersion(ctx)
	status.KubeletRunning = utils.IsServiceActive("kubelet")
	status.KubeletReady = c.isKubeletReady(ctx)

	// get containerd related status
	status.ContainerdVersion = c.getContainerdVersion(ctx)
	// check if containerd is running, it will cause kubelet not ready
	status.ContainerdRunning = utils.IsServiceActive("containerd")

	// Get runc version
	status.RuncVersion = c.getRuncVersion(ctx)

	// Collect Arc status
	arcStatus, err := c.collectArcStatus(ctx)
	if err != nil {
		c.logger.Warnf("Failed to collect Arc status: %v", err)
	}
	status.ArcStatus = arcStatus

	return status, nil
}

// getKubeletVersion gets the kubelet version
func (c *Collector) getKubeletVersion(ctx context.Context) string {
	output, err := c.runCommand(ctx, "/usr/local/bin/kubelet", "--version")
	if err != nil {
		c.logger.Warnf("Failed to get kubelet version: %v", err)
		return "unknown"
	}

	// Extract version from output like "Kubernetes v1.32.7"
	parts := strings.Fields(strings.TrimSpace(output))
	if len(parts) >= 2 {
		return strings.TrimPrefix(parts[1], "v")
	}

	c.logger.Warnf("Failed to parse kubelet version from output: %s", output)
	return "unknown"
}

func (c *Collector) getContainerdVersion(ctx context.Context) string {
	output, err := c.runCommand(ctx, "containerd", "--version")
	if err != nil {
		c.logger.Warnf("Failed to get containerd version: %v", err)
		return "unknown"
	}

	// Extract version from output like "containerd github.com/containerd/containerd v1.7.20 8fc6bcff51318944179630522a095cc9dbf9f353"
	parts := strings.Fields(strings.TrimSpace(output))
	if len(parts) >= 3 {
		return strings.TrimPrefix(parts[2], "v")
	}
	return "unknown"
}

// getRuncVersion gets the runc version
func (c *Collector) getRuncVersion(ctx context.Context) string {
	output, err := c.runCommand(ctx, "runc", "--version")
	if err != nil {
		c.logger.Warnf("Failed to get runc version: %v", err)
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

	c.logger.Warnf("Failed to parse runc version from output: %s", output)
	return "unknown"
}

// collectArcStatus gathers Azure Arc machine registration and connection status
func (c *Collector) collectArcStatus(ctx context.Context) (ArcStatus, error) {
	status := ArcStatus{}

	// Try to get comprehensive Arc status from azcmagent show
	if output, err := c.runCommand(ctx, "azcmagent", "show"); err == nil {
		c.parseArcShowOutput(&status, output)
	} else {
		// If azcmagent show fails, explicitly mark as disconnected
		c.logger.Debugf("azcmagent show failed: %v - marking Arc as disconnected", err)
		status.Connected = false
		status.Registered = false
	}

	return status, nil
}

// runCommand executes a system command and returns the output with a timeout
func (c *Collector) runCommand(ctx context.Context, name string, args ...string) (string, error) {
	// Create a context with timeout to prevent hanging commands
	timeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, name, args...)
	output, err := cmd.Output()
	return string(output), err
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
			status.Registered = status.Connected // If connected, assume registered
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
		c.logger.Warnf("Failed to get hostname: %v", err)
		return "Unknown"
	}

	// Readiness condition status is one of: True, False, Unknown
	args := []string{
		"--kubeconfig",
		kubelet.KubeletKubeconfigPath,
		"get",
		"node",
		hostName,
		"-o",
		"jsonpath={.status.conditions[?(@.type==\"Ready\")].status}",
	}

	output, err := utils.RunCommandWithOutput("kubectl", args...)
	if err != nil {
		// Common in dev: agent runs as ubuntu and can't read root:aks-flex-node 0640 kubeconfig.
		// Retry with sudo (non-interactive) if we see a permissions failure.
		c.logger.Errorf("kubectl command failed: %v with output: %s", err, output)
		return "Unknown"
	}

	switch strings.TrimSpace(output) {
	case "True":
		return "Ready"
	case "False":
		return "NotReady"
	default:
		return "Unknown"
	}
}

// NeedsBootstrap checks if the node needs to be (re)bootstrapped based on status file
func (c *Collector) NeedsBootstrap(ctx context.Context) bool {
	statusFilePath := GetStatusFilePath()
	// Try to read the status file
	statusData, err := os.ReadFile(statusFilePath)
	if err != nil {
		c.logger.Info("Status file not found - bootstrap needed")
		return true
	}

	var nodeStatus NodeStatus
	if err := json.Unmarshal(statusData, &nodeStatus); err != nil {
		c.logger.Info("Could not parse status file - bootstrap needed")
		return true
	}

	// Check if status indicates unhealthy conditions
	if !nodeStatus.KubeletRunning {
		c.logger.Info("Status file indicates kubelet not running - bootstrap needed")
		return true
	}

	// Check if Arc status is unhealthy (if configured)
	if c.config != nil && c.config.GetArcMachineName() != "" {
		if !nodeStatus.ArcStatus.Connected {
			c.logger.Info("Status file indicates Arc agent not connected - bootstrap needed")
			return true
		}
	}

	// Check if status is too old (older than 5 minutes might indicate daemon issues)
	if time.Since(nodeStatus.LastUpdated) > 5*time.Minute {
		c.logger.Info("Status file is stale (older than 5 minutes) - bootstrap needed")
		return true
	}

	// Check for essential component versions being unknown (indicates collection failures)
	if nodeStatus.KubeletVersion == "unknown" || nodeStatus.KubeletVersion == "" {
		c.logger.Info("Status file indicates kubelet version unknown - bootstrap needed")
		return true
	}

	if nodeStatus.RuncVersion == "unknown" || nodeStatus.RuncVersion == "" {
		c.logger.Info("Status file indicates runc version unknown - bootstrap needed")
		return true
	}

	c.logger.Debug("Status file indicates healthy state - no bootstrap needed")
	return false
}

// GetStatusFilePath returns the appropriate status directory path
// Uses /run/aks-flex-node/status.json when running as aks-flex-node user (systemd service)
// Uses /tmp/aks-flex-node/status.json for direct user execution (testing/development)
func GetStatusFilePath() string {
	// Running as regular user (testing/development) - use temp directory
	statusDir := "/tmp/aks-flex-node"
	// Check if we're running as the aks-flex-node service user
	currentUser, err := user.Current()
	if err == nil && currentUser.Username == "aks-flex-node" {
		// Running as systemd service user - use runtime directory for status files
		statusDir = "/run/aks-flex-node"
	}

	return filepath.Join(statusDir, "status.json")
}
