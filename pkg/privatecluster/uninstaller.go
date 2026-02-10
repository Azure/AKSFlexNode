package privatecluster

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
)

// Uninstaller handles private cluster uninstallation
type Uninstaller struct {
	logger        *Logger
	azureClient   *AzureClient
	toolInstaller *ToolInstaller
	options       UninstallOptions

	// State
	clusterInfo *AKSClusterInfo
	vpnConfig   VPNConfig
	sshKeyPath  string
	gatewayIP   string
	clientKey   string
}

// NewUninstaller creates a new Uninstaller instance.
// cred is the Azure credential used for SDK calls. If nil, Azure resource cleanup will be skipped.
func NewUninstaller(options UninstallOptions, cred azcore.TokenCredential) (*Uninstaller, error) {
	logger := NewLogger(false)

	u := &Uninstaller{
		logger:        logger,
		toolInstaller: NewToolInstaller(logger),
		options:       options,
		vpnConfig:     DefaultVPNConfig(),
		sshKeyPath:    GetSSHKeyPath(),
	}

	// Only create Azure client if we have a resource ID (needed for full cleanup)
	if options.AKSResourceID != "" && cred != nil {
		subscriptionID, _, _, err := ParseResourceID(options.AKSResourceID)
		if err != nil {
			return nil, fmt.Errorf("failed to parse resource ID: %w", err)
		}
		azureClient, err := NewAzureClient(cred, subscriptionID, logger)
		if err != nil {
			return nil, fmt.Errorf("failed to create Azure client: %w", err)
		}
		u.azureClient = azureClient
	}

	return u, nil
}

// Uninstall runs the uninstallation process
func (u *Uninstaller) Uninstall(ctx context.Context) error {
	fmt.Printf("%sRemove Edge Node from Private AKS Cluster%s\n", colorYellow, colorReset)
	fmt.Printf("%s=====================================%s\n\n", colorYellow, colorReset)

	// Parse resource ID if provided
	if u.options.AKSResourceID != "" {
		subscriptionID, resourceGroup, clusterName, err := ParseResourceID(u.options.AKSResourceID)
		if err != nil {
			return err
		}
		u.clusterInfo = &AKSClusterInfo{
			ResourceID:     u.options.AKSResourceID,
			SubscriptionID: subscriptionID,
			ResourceGroup:  resourceGroup,
			ClusterName:    clusterName,
		}
		u.logger.Info("Cluster: %s/%s (Subscription: %s)", resourceGroup, clusterName, subscriptionID)
	}

	_ = u.toolInstaller.InstallConnectedMachineExtension(ctx)

	switch u.options.Mode {
	case CleanupModeLocal:
		return u.cleanupLocal(ctx)
	case CleanupModeFull:
		return u.cleanupFull(ctx)
	default:
		return fmt.Errorf("invalid cleanup mode: %s", u.options.Mode)
	}
}

// cleanupLocal performs local cleanup (keeps Gateway)
func (u *Uninstaller) cleanupLocal(ctx context.Context) error {
	u.logger.Info("Performing local cleanup (keeping Gateway)...")

	hostname, err := GetHostname()
	if err != nil {
		return err
	}

	// Get Gateway IP and client key from VPN config (before stopping VPN)
	u.readVPNConfig()

	// Remove node from cluster while VPN is still connected.
	// This must happen here (not in bootstrapper) because the private cluster API server
	// is only reachable through the VPN tunnel, which gets torn down below.
	u.removeNodeFromCluster(ctx, hostname)

	// Note: stopFlexNodeAgent and removeArcAgent are handled by the bootstrapper's
	// services.UnInstaller and arc.UnInstaller steps respectively.

	// Remove client peer from Gateway
	u.removeClientPeerFromGateway(ctx)

	// Stop VPN
	u.stopVPN(ctx)

	// Delete VPN client configuration
	u.deleteVPNConfig()

	// Clean up hosts entries
	u.cleanupHostsEntries()

	// Note: config.json is preserved for potential re-use

	fmt.Println()
	u.logger.Success("Local cleanup completed!")
	fmt.Println()
	fmt.Println("To rejoin cluster, run:")
	fmt.Println("  sudo ./aks-flex-node agent --config config.json  # with private: true")

	return nil
}

// cleanupFull performs full cleanup (removes all Azure resources)
func (u *Uninstaller) cleanupFull(ctx context.Context) error {
	u.logger.Info("Performing full cleanup...")

	hostname, err := GetHostname()
	if err != nil {
		return err
	}

	// Get Gateway IP and client key from VPN config (before stopping VPN)
	u.readVPNConfig()

	// Remove node from cluster while VPN is still connected (see comment in cleanupLocal)
	u.removeNodeFromCluster(ctx, hostname)

	// Remove client peer from Gateway
	u.removeClientPeerFromGateway(ctx)

	// Stop VPN
	u.stopVPN(ctx)

	// Delete VPN client configuration
	u.deleteVPNConfig()

	// Clean up hosts entries
	u.cleanupHostsEntries()

	// Delete Azure resources
	if err := u.deleteAzureResources(ctx); err != nil {
		u.logger.Warning("Failed to delete some Azure resources: %v", err)
	}

	// Delete SSH keys
	u.deleteSSHKeys()

	// Note: config.json is preserved for potential re-use

	fmt.Println()
	u.logger.Success("Full cleanup completed!")
	fmt.Println()
	fmt.Println("All components and Azure resources have been removed.")
	fmt.Println("The local machine is now clean.")

	return nil
}

// readVPNConfig reads Gateway IP and client key from VPN config
func (u *Uninstaller) readVPNConfig() {
	vpnClient := NewVPNClient(u.vpnConfig, u.logger)
	gatewayIP, clientKey, err := vpnClient.GetClientConfigInfo()
	if err == nil {
		u.gatewayIP = gatewayIP
		u.clientKey = clientKey
	}
}

// removeNodeFromCluster removes the node from the Kubernetes cluster
func (u *Uninstaller) removeNodeFromCluster(ctx context.Context, nodeName string) {
	if !CommandExists("kubectl") {
		return
	}

	u.logger.Info("Removing node %s from cluster...", nodeName)

	// Try root kubeconfig first
	if _, err := RunCommand(ctx, "kubectl", "--kubeconfig", "/root/.kube/config",
		"delete", "node", nodeName, "--ignore-not-found"); err == nil {
		u.logger.Success("Node removed from cluster")
		return
	}

	// Try default kubeconfig
	if _, err := RunCommand(ctx, "kubectl", "delete", "node", nodeName, "--ignore-not-found"); err == nil {
		u.logger.Success("Node removed from cluster")
		return
	}

	u.logger.Warning("Failed to remove node from cluster (may need manual cleanup: kubectl delete node %s)", nodeName)
}

// removeClientPeerFromGateway removes this client's peer from the Gateway
func (u *Uninstaller) removeClientPeerFromGateway(ctx context.Context) {
	if u.gatewayIP == "" || u.clientKey == "" || !FileExists(u.sshKeyPath) {
		return
	}

	u.logger.Info("Removing client peer from Gateway...")

	// Get public key from private key
	vpnClient := NewVPNClient(u.vpnConfig, u.logger)
	clientPubKey, err := vpnClient.GetPublicKeyFromPrivate(ctx, u.clientKey)
	if err != nil || clientPubKey == "" {
		return
	}

	// Connect to Gateway and remove peer
	sshConfig := DefaultSSHConfig(u.sshKeyPath, u.gatewayIP)
	sshConfig.Timeout = 10
	ssh := NewSSHClient(sshConfig, u.logger)
	vpnServer := NewVPNServerManager(ssh, u.logger)

	_ = vpnServer.RemovePeer(ctx, clientPubKey)
	u.logger.Success("Client peer removed from Gateway")
}

// stopVPN stops the VPN connection
func (u *Uninstaller) stopVPN(ctx context.Context) {
	vpnClient := NewVPNClient(u.vpnConfig, u.logger)
	_ = vpnClient.Stop(ctx)
	u.logger.Success("VPN connection stopped")
}

// deleteVPNConfig deletes the VPN client configuration
func (u *Uninstaller) deleteVPNConfig() {
	vpnClient := NewVPNClient(u.vpnConfig, u.logger)
	_ = vpnClient.RemoveClientConfig()
	u.logger.Success("VPN config deleted")
}

// cleanupHostsEntries removes AKS-related entries from /etc/hosts
func (u *Uninstaller) cleanupHostsEntries() {
	_ = RemoveHostsEntries("privatelink")
	_ = RemoveHostsEntries("azmk8s.io")
	u.logger.Success("Hosts entries cleaned")
}

// deleteSSHKeys deletes the Gateway SSH keys
func (u *Uninstaller) deleteSSHKeys() {
	_ = RemoveSSHKeys(u.sshKeyPath)
	u.logger.Success("SSH keys deleted")
}

// deleteAzureResources deletes all Azure resources created for the Gateway
func (u *Uninstaller) deleteAzureResources(ctx context.Context) error {
	if u.clusterInfo == nil || u.azureClient == nil {
		return fmt.Errorf("cluster info or Azure client not available")
	}

	u.logger.Info("Deleting Azure resources...")

	gatewayName := "wg-gateway"
	nicName := gatewayName + "VMNic"
	pipName := gatewayName + "-pip"
	nsgName := gatewayName + "-nsg"

	if err := u.azureClient.DeleteVM(ctx, u.clusterInfo.ResourceGroup, gatewayName); err != nil {
		u.logger.Warning("Delete VM: %v", err)
	}
	if err := u.azureClient.DeleteNIC(ctx, u.clusterInfo.ResourceGroup, nicName); err != nil {
		u.logger.Warning("Delete NIC: %v", err)
	}
	if err := u.azureClient.DeletePublicIP(ctx, u.clusterInfo.ResourceGroup, pipName); err != nil {
		u.logger.Warning("Delete Public IP: %v", err)
	}
	if err := u.azureClient.DeleteNSG(ctx, u.clusterInfo.ResourceGroup, nsgName); err != nil {
		u.logger.Warning("Delete NSG: %v", err)
	}
	_ = u.azureClient.DeleteDisks(ctx, u.clusterInfo.ResourceGroup, gatewayName)

	clusterInfo, err := u.azureClient.GetAKSClusterInfo(ctx, u.clusterInfo.ResourceGroup, u.clusterInfo.ClusterName)
	if err == nil {
		vnetName, vnetRG, err := u.azureClient.GetVNetInfo(ctx, clusterInfo.NodeResourceGroup)
		if err == nil {
			_ = u.azureClient.DeleteSubnet(ctx, vnetRG, vnetName, "wg-subnet")
		}
	}
	u.logger.Success("Azure resources deleted")

	return nil
}
