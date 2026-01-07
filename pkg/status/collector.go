package status

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
)

// Collector collects system and node status information
type Collector struct {
	config *config.Config
	logger *logrus.Logger
}

// NewCollector creates a new status collector
func NewCollector(cfg *config.Config, logger *logrus.Logger) *Collector {
	return &Collector{
		config: cfg,
		logger: logger,
	}
}

// CollectStatus collects essential node status information
func (c *Collector) CollectStatus(ctx context.Context) (*NodeStatus, error) {
	status := &NodeStatus{
		LastUpdated:  time.Now(),
		AgentVersion: "", // Will be set by caller
	}

	// Get kubelet version
	if version, err := c.getKubeletVersion(ctx); err == nil {
		status.KubeletVersion = version
	} else {
		c.logger.Warnf("Failed to get kubelet version: %v", err)
		status.KubeletVersion = "unknown"
	}

	// Check if kubelet is running
	status.KubeletRunning = c.isKubeletRunning(ctx)

	// Get runc version
	if version, err := c.getRuncVersion(ctx); err == nil {
		status.RuncVersion = version
	} else {
		c.logger.Warnf("Failed to get runc version: %v", err)
		status.RuncVersion = "unknown"
	}

	// Collect Arc status
	arcStatus, err := c.collectArcStatus(ctx)
	if err != nil {
		c.logger.Warnf("Failed to collect Arc status: %v", err)
	}
	status.ArcStatus = arcStatus

	return status, nil
}

// getKubeletVersion gets the kubelet version
func (c *Collector) getKubeletVersion(ctx context.Context) (string, error) {
	output, err := c.runCommand(ctx, "/usr/local/bin/kubelet", "--version")
	if err != nil {
		return "", err
	}

	// Extract version from output like "Kubernetes v1.32.7"
	parts := strings.Fields(strings.TrimSpace(output))
	if len(parts) >= 2 {
		return strings.TrimPrefix(parts[1], "v"), nil
	}

	return "", fmt.Errorf("could not parse kubelet version from: %s", output)
}

// getRuncVersion gets the runc version
func (c *Collector) getRuncVersion(ctx context.Context) (string, error) {
	output, err := c.runCommand(ctx, "runc", "--version")
	if err != nil {
		return "", err
	}

	// Parse runc version output
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for _, line := range lines {
		if strings.Contains(line, "version") {
			parts := strings.Fields(line)
			for i, part := range parts {
				if part == "version" && i+1 < len(parts) {
					return parts[i+1], nil
				}
			}
		}
	}

	return "", fmt.Errorf("could not parse runc version from: %s", output)
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

	// If config is available, use it to override/fill missing values
	if c.config != nil {
		if status.MachineName == "" {
			status.MachineName = c.config.GetArcMachineName()
		}
		if status.Location == "" {
			status.Location = c.config.GetArcLocation()
		}
		if status.ResourceGroup == "" {
			status.ResourceGroup = c.config.GetArcResourceGroup()
		}
	}

	// If we couldn't get status from azcmagent show, try fallback methods
	if !status.Connected {
		if connected, err := c.checkArcConnectivityFallback(ctx); err == nil {
			status.Connected = connected
			status.Registered = connected // Simplified: if connected, assume registered
		}
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
		case "Agent Version":
			status.AgentVersion = value
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

// checkArcConnectivityFallback checks if the Arc agent is connected to Azure using fallback methods
func (c *Collector) checkArcConnectivityFallback(ctx context.Context) (bool, error) {
	// Method 1: Check if azcmagent shows status as connected
	if output, err := c.runCommand(ctx, "azcmagent", "show"); err == nil {
		// Look for "Agent Status" field in the output
		lines := strings.Split(strings.TrimSpace(output), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if strings.Contains(line, "Agent Status") && strings.Contains(line, ":") {
				// Parse the status value after the colon
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					status := strings.TrimSpace(parts[1])
					return strings.ToLower(status) == "connected", nil
				}
			}
		}
	}

	// Method 2: Check if himds service is running (fallback)
	if output, err := c.runCommand(ctx, "systemctl", "is-active", "himds"); err == nil {
		return strings.TrimSpace(output) == "active", nil
	}

	return false, nil
}


// isKubeletRunning checks if the kubelet service is running
func (c *Collector) isKubeletRunning(ctx context.Context) bool {
	if output, err := c.runCommand(ctx, "systemctl", "is-active", "kubelet"); err == nil {
		return strings.TrimSpace(output) == "active"
	}
	return false
}


// NeedsBootstrap checks if the node needs to be (re)bootstrapped based on status file
func (c *Collector) NeedsBootstrap(ctx context.Context) bool {
	statusFilePath := "/tmp/aks-flex-node/status.json"

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

	// Check if status is too old (older than 15 minutes might indicate daemon issues)
	if time.Since(nodeStatus.LastUpdated) > 15*time.Minute {
		c.logger.Info("Status file is stale (older than 15 minutes) - bootstrap needed")
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
