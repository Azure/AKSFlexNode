package vpn_gateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v4"
	"github.com/sirupsen/logrus"

	"go.goms.io/aks/AKSFlexNode/pkg/auth"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
)

// UnInstaller handles VPN Gateway cleanup operations
type UnInstaller struct {
	config         *config.Config
	logger         *logrus.Logger
	vnetClient     *armnetwork.VirtualNetworksClient
	subnetsClient  *armnetwork.SubnetsClient
	vgwClient      *armnetwork.VirtualNetworkGatewaysClient
	publicIPClient *armnetwork.PublicIPAddressesClient
}

// NewUnInstaller creates a new VPN Gateway uninstaller
func NewUnInstaller(logger *logrus.Logger) *UnInstaller {
	cfg := config.GetConfig()
	return &UnInstaller{
		config: cfg,
		logger: logger,
	}
}

// GetName returns the cleanup step name
func (u *UnInstaller) GetName() string {
	return "VPNGatewayCleanup"
}

// Execute performs VPN Gateway cleanup as part of the unbootstrap process
// This method is resilient to failures and continues cleanup even if some operations fail
func (u *UnInstaller) Execute(ctx context.Context) error {
	u.logger.Info("Starting VPN Gateway cleanup for unbootstrap process")

	// Set up Azure clients
	if err := u.setUpClients(ctx); err != nil {
		u.logger.Errorf("Failed to set up Azure clients: %v", err)
		return fmt.Errorf("vpn gateway setup failed at client setup: %w", err)
	}

	// Step 1: Disconnect VPN connection
	u.logger.Info("Step 1: Disconnecting VPN connection")
	if err := u.disconnectVPN(); err != nil {
		u.logger.Warnf("Failed to disconnect VPN (continuing cleanup): %v", err)
	} else {
		u.logger.Info("Successfully disconnected VPN connection")
	}

	// Step 2: Clean up VPN networking (routes and iptables rules)
	u.logger.Info("Step 2: Cleaning up VPN networking configuration")
	if err := u.cleanupVPNNetworking(); err != nil {
		u.logger.Warnf("Failed to cleanup VPN networking (continuing cleanup): %v", err)
	} else {
		u.logger.Info("Successfully cleaned up VPN networking configuration")
	}

	// Step 3: Clean up VPN configuration files and certificates
	u.logger.Info("Step 3: Cleaning up VPN configuration files and certificates")
	if err := u.cleanupVPNFiles(); err != nil {
		u.logger.Warnf("Failed to cleanup VPN files (continuing cleanup): %v", err)
	} else {
		u.logger.Info("Successfully cleaned up VPN configuration files and certificates")
	}

	// Note: We don't delete the VPN Gateway from Azure as it's expensive to recreate
	// and might be shared with other resources. The VPN Gateway will be left in Azure.
	u.logger.Info("VPN Gateway resources in Azure are preserved for potential reuse")

	if err := u.cleanupAzureResources(ctx); err != nil {
		u.logger.Warnf("Failed to cleanup Azure VPN Gateway resources: %v", err)
	} else {
		u.logger.Info("Successfully cleaned up Azure VPN Gateway resources")
	}

	u.logger.Info("VPN Gateway cleanup for unbootstrap completed")
	return nil
}

// setUpAKSClients sets up Azure Container Service clients using the target cluster subscription ID
func (u *UnInstaller) setUpClients(ctx context.Context) error {
	cred, err := auth.NewAuthProvider().UserCredential(config.GetConfig())
	if err != nil {
		return fmt.Errorf("failed to get authentication credential: %w", err)
	}

	vnetID := u.config.GetVPNGatewayVNetID()
	if vnetID == "" {
		return fmt.Errorf("failed to get VNet ID from configuration")
	}
	vnetSub := utils.GetSubscriptionIDFromResourceID(vnetID)

	clientFactory, err := armnetwork.NewClientFactory(vnetSub, cred, nil)
	if err != nil {
		return fmt.Errorf("failed to create Azure Network client factory: %w", err)
	}

	u.vnetClient = clientFactory.NewVirtualNetworksClient()
	u.subnetsClient = clientFactory.NewSubnetsClient()
	u.vgwClient = clientFactory.NewVirtualNetworkGatewaysClient()
	u.publicIPClient = clientFactory.NewPublicIPAddressesClient()
	return nil
}

// IsCompleted checks if VPN Gateway cleanup has been completed
func (u *UnInstaller) IsCompleted(ctx context.Context) bool {
	if !u.config.IsVPNGatewayEnabled() {
		u.logger.Info("VPN Gateway is not enabled in configuration; skipping cleanup")
		return true
	}
	return false
}

// disconnectVPN stops the VPN connection and OpenVPN service
func (u *UnInstaller) disconnectVPN() error {
	u.logger.Info("Disconnecting VPN connection")

	// Stop OpenVPN service
	if err := utils.StopService(openVPNServiceTemplate); err != nil {
		u.logger.Warnf("Failed to stop OpenVPN service: %v", err)
		// Continue with other cleanup steps
	}

	// Kill any remaining OpenVPN processes
	if err := utils.RunSystemCommand("pkill", "-f", "openvpn"); err != nil {
		u.logger.Warnf("Failed to kill OpenVPN processes: %v", err)
		// Continue with other cleanup steps
	}

	u.logger.Info("VPN disconnection completed")
	return nil
}

// cleanupVPNFiles removes VPN configuration files and certificates
func (u *UnInstaller) cleanupVPNFiles() error {
	u.logger.Info("Cleaning up VPN configuration files and certificates")

	// Use the utility function for file removal
	filesToCleanup := []string{
		GetVPNConfigPath(),
		GetVPNClientCertPath(),
		GetVPNClientKeyPath(),
		GetVPNRootCertPath(),
	}

	if errors := utils.RemoveFiles(filesToCleanup, u.logger); len(errors) > 0 {
		for _, err := range errors {
			u.logger.Warnf("File removal error: %v", err)
		}
	}

	// Try to remove the certificates directory using the utility function
	if errors := utils.RemoveDirectories([]string{certificatesDir}, u.logger); len(errors) > 0 {
		for _, err := range errors {
			u.logger.Debugf("Directory removal error: %v", err)
		}
	}

	u.logger.Info("VPN files and certificates cleanup completed")
	return nil
}

// cleanupVPNNetworking removes IP routes and iptables rules configured for VPN
func (u *UnInstaller) cleanupVPNNetworking() error {
	u.logger.Info("Cleaning up VPN networking configuration (routes and iptables rules)")

	// Get VPN interface to clean up routes
	vpnInterface, err := utils.GetVPNInterface()
	if err != nil {
		u.logger.Infof("No VPN interface found, skipping route cleanup: %v", err)
		// Continue with iptables cleanup even if no VPN interface
	} else {
		u.logger.Infof("Found VPN interface: %s, cleaning up routes", vpnInterface)

		// Get all routes via the VPN interface and remove them
		if err := u.cleanupVPNRoutes(vpnInterface); err != nil {
			u.logger.Warnf("Failed to clean up VPN routes: %v", err)
		}
	}

	// Clean up iptables MASQUERADE rules
	if err := u.cleanupIPTablesRules(); err != nil {
		u.logger.Warnf("Failed to clean up iptables rules: %v", err)
	}

	u.logger.Info("VPN networking cleanup completed")
	return nil
}

// cleanupVPNRoutes removes all routes that go through the VPN interface
func (u *UnInstaller) cleanupVPNRoutes(vpnInterface string) error {
	u.logger.Infof("Cleaning up routes via interface: %s", vpnInterface)

	// Get current routing table
	output, err := utils.RunCommandWithOutput("ip", "route", "show")
	if err != nil {
		return fmt.Errorf("failed to get current routes: %w", err)
	}

	// Parse routes and find ones using our VPN interface
	routes := strings.Split(output, "\n")
	routesRemoved := 0

	for _, route := range routes {
		route = strings.TrimSpace(route)
		if route == "" {
			continue
		}

		// Check if this route uses our VPN interface
		if strings.Contains(route, "dev "+vpnInterface) {
			// Extract the destination from the route (first part before whitespace)
			parts := strings.Fields(route)
			if len(parts) > 0 {
				dest := parts[0]
				u.logger.Infof("Removing route: %s", dest)

				// Remove the route
				if err := utils.RunSystemCommand("ip", "route", "del", dest); err != nil {
					u.logger.Warnf("Failed to remove route %s: %v", dest, err)
					// Continue with other routes
				} else {
					routesRemoved++
					u.logger.Infof("Removed route: %s", dest)
				}
			}
		}
	}

	u.logger.Infof("Removed %d VPN routes", routesRemoved)
	return nil
}

// cleanupIPTablesRules removes iptables MASQUERADE rules added for VPN
func (u *UnInstaller) cleanupIPTablesRules() error {
	u.logger.Info("Cleaning up iptables MASQUERADE rules")

	// Get current NAT table rules
	output, err := utils.RunCommandWithOutput("iptables", "-t", "nat", "-L", "POSTROUTING", "-n", "--line-numbers")
	if err != nil {
		return fmt.Errorf("failed to list iptables rules: %w", err)
	}

	// Parse output to find MASQUERADE rules
	lines := strings.Split(output, "\n")
	rulesRemoved := 0

	// Process lines in reverse order to maintain line numbers when deleting
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "Chain") || strings.HasPrefix(line, "num") {
			continue
		}

		// Look for MASQUERADE rules (likely involving our VPN interface or VNet CIDRs)
		if strings.Contains(line, "MASQUERADE") {
			// Extract the line number (first field)
			parts := strings.Fields(line)
			if len(parts) > 0 {
				lineNum := parts[0]

				// Check if this rule involves VPN (look for tun interface references)
				if strings.Contains(line, "tun") {
					u.logger.Infof("Removing iptables MASQUERADE rule: %s", line)

					// Remove the rule by line number
					if err := utils.RunSystemCommand("iptables", "-t", "nat", "-D", "POSTROUTING", lineNum); err != nil {
						u.logger.Warnf("Failed to remove iptables rule %s: %v", lineNum, err)
						// Continue with other rules
					} else {
						rulesRemoved++
						u.logger.Infof("Removed iptables rule: %s", line)
					}
				}
			}
		}
	}

	u.logger.Infof("Removed %d iptables MASQUERADE rules", rulesRemoved)
	return nil
}

// cleanupAzureResources removes VPN Gateway resources from Azure
// This includes: VPN Gateway, Public IP, and GatewaySubnet
func (u *UnInstaller) cleanupAzureResources(ctx context.Context) error {
	u.logger.Info("Cleaning up VPN Gateway resources from Azure")

	vnetID := u.config.GetVPNGatewayVNetID()
	resourceGroupName := utils.GetResourceGroupFromResourceID(vnetID)
	vnetName := utils.GetResourceNameFromResourceID(vnetID)

	// Step 1: Delete VPN Gateway (this must be done first as it depends on other resources)
	u.logger.Infof("Deleting VPN Gateway: %s (this may take 10-20 minutes)", defaultVPNGatewayName)
	if err := u.deleteVPNGateway(ctx, resourceGroupName); err != nil {
		u.logger.Warnf("Failed to delete VPN Gateway: %v", err)
		// Continue with other cleanup even if VPN Gateway deletion fails
	} else {
		u.logger.Info("VPN Gateway successfully deleted from Azure")
	}

	// Step 2: Delete Public IP
	u.logger.Infof("Deleting Public IP: %s", gatewayPublicIPName)
	if err := u.deletePublicIP(ctx, resourceGroupName); err != nil {
		u.logger.Warnf("Failed to delete Public IP: %v", err)
		// Continue with other cleanup
	} else {
		u.logger.Info("Public IP successfully deleted from Azure")
	}

	// Step 3: Delete GatewaySubnet (this should be done last)
	u.logger.Infof("Deleting GatewaySubnet: %s", gatewaySubnetName)
	if err := u.deleteGatewaySubnet(ctx, resourceGroupName, vnetName); err != nil {
		u.logger.Warnf("Failed to delete GatewaySubnet: %v", err)
		// Continue - this is not critical
	} else {
		u.logger.Info("GatewaySubnet successfully deleted from Azure")
	}

	u.logger.Info("Azure VPN Gateway resources cleanup completed")
	return nil
}

// deleteVPNGateway deletes the VPN Gateway
func (u *UnInstaller) deleteVPNGateway(ctx context.Context, resourceGroupName string) error {
	// Check if VPN Gateway exists before trying to delete
	gateway, err := u.vgwClient.Get(ctx, resourceGroupName, defaultVPNGatewayName, nil)
	if err != nil {
		// If gateway doesn't exist, consider it already deleted
		u.logger.Infof("VPN Gateway %s not found, may already be deleted", defaultVPNGatewayName)
		return nil
	}

	if gateway.Properties == nil || gateway.Properties.ProvisioningState == nil {
		u.logger.Warn("VPN Gateway found but has incomplete properties")
		return nil
	}

	u.logger.Infof("Found VPN Gateway %s in state: %s", defaultVPNGatewayName, *gateway.Properties.ProvisioningState)

	poller, err := u.vgwClient.BeginDelete(ctx, resourceGroupName, defaultVPNGatewayName, nil)
	if err != nil {
		return fmt.Errorf("failed to start VPN Gateway deletion: %w", err)
	}

	// Wait for deletion to complete
	_, err = poller.PollUntilDone(ctx, nil)
	if err != nil {
		return fmt.Errorf("VPN Gateway deletion failed: %w", err)
	}

	return nil
}

// deletePublicIP deletes the Public IP used by VPN Gateway
func (u *UnInstaller) deletePublicIP(ctx context.Context, resourceGroupName string) error {
	// Check if Public IP exists before trying to delete
	_, err := u.publicIPClient.Get(ctx, resourceGroupName, gatewayPublicIPName, nil)
	if err != nil {
		// If Public IP doesn't exist, consider it already deleted
		u.logger.Infof("Public IP %s not found, may already be deleted", gatewayPublicIPName)
		return nil
	}

	poller, err := u.publicIPClient.BeginDelete(ctx, resourceGroupName, gatewayPublicIPName, nil)
	if err != nil {
		return fmt.Errorf("failed to start Public IP deletion: %w", err)
	}

	// Wait for deletion to complete
	_, err = poller.PollUntilDone(ctx, nil)
	if err != nil {
		return fmt.Errorf("public IP deletion failed: %w", err)
	}

	return nil
}

// deleteGatewaySubnet deletes the GatewaySubnet
func (u *UnInstaller) deleteGatewaySubnet(ctx context.Context, resourceGroupName, vnetName string) error {
	// Check if GatewaySubnet exists before trying to delete
	_, err := u.subnetsClient.Get(ctx, resourceGroupName, vnetName, gatewaySubnetName, nil)
	if err != nil {
		// If subnet doesn't exist, consider it already deleted
		u.logger.Infof("GatewaySubnet %s not found, may already be deleted", gatewaySubnetName)
		return nil
	}

	poller, err := u.subnetsClient.BeginDelete(ctx, resourceGroupName, vnetName, gatewaySubnetName, nil)
	if err != nil {
		return fmt.Errorf("failed to start GatewaySubnet deletion: %w", err)
	}

	// Wait for deletion to complete
	_, err = poller.PollUntilDone(ctx, nil)
	if err != nil {
		return fmt.Errorf("GatewaySubnet deletion failed: %w", err)
	}

	return nil
}
