package privatecluster

import (
	"context"
	"fmt"

	"github.com/sirupsen/logrus"

	"go.goms.io/aks/AKSFlexNode/pkg/auth"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
)

// Uninstaller handles private cluster VPN/Gateway teardown, implementing bootstrapper.Executor.
type Uninstaller struct {
	config       *config.Config
	logger       *logrus.Logger
	authProvider *auth.AuthProvider
	azureClient  *AzureClient

	clusterInfo *AKSClusterInfo
	vpnConfig   VPNConfig
	sshKeyPath  string
	gatewayIP   string
	clientKey   string
}

// NewUninstaller creates a new private cluster Uninstaller.
func NewUninstaller(logger *logrus.Logger) *Uninstaller {
	return &Uninstaller{
		config:       config.GetConfig(),
		logger:       logger,
		authProvider: auth.NewAuthProvider(),
		vpnConfig:    DefaultVPNConfig(),
		sshKeyPath:   GetSSHKeyPath(),
	}
}

// GetName returns the step name.
func (u *Uninstaller) GetName() string {
	return "PrivateClusterUninstall"
}

// IsCompleted returns true for non-private clusters; always false for private clusters.
func (u *Uninstaller) IsCompleted(ctx context.Context) bool {
	if !u.isPrivateCluster() {
		return true
	}
	return false // Always attempt cleanup for private clusters
}

// Execute runs the private cluster uninstallation.
func (u *Uninstaller) Execute(ctx context.Context) error {
	if !u.isPrivateCluster() {
		return nil
	}

	u.logger.Infof("Remove Edge Node from Private AKS Cluster")
	u.logger.Infof("=====================================")

	cleanupMode := u.config.Azure.TargetCluster.CleanupMode
	var mode CleanupMode
	switch cleanupMode {
	case "local", "":
		mode = CleanupModeLocal
	case "full":
		mode = CleanupModeFull
	default:
		return fmt.Errorf("invalid cleanup mode: %s (use 'local' or 'full')", cleanupMode)
	}

	resourceID := u.config.GetTargetClusterID()
	if resourceID != "" {
		subscriptionID := u.config.GetTargetClusterSubscriptionID()
		resourceGroup := u.config.GetTargetClusterResourceGroup()
		clusterName := u.config.GetTargetClusterName()
		u.clusterInfo = &AKSClusterInfo{
			ResourceID:     resourceID,
			SubscriptionID: subscriptionID,
			ResourceGroup:  resourceGroup,
			ClusterName:    clusterName,
		}
		u.logger.Infof("Cluster: %s/%s (Subscription: %s)", resourceGroup, clusterName, subscriptionID)

		if mode == CleanupModeFull {
			cred, err := u.authProvider.UserCredential(u.config)
			if err != nil {
				u.logger.Warnf("Failed to get Azure credential: %v", err)
			} else {
				azureClient, err := NewAzureClient(cred, subscriptionID, u.logger)
				if err != nil {
					u.logger.Warnf("Failed to create Azure client: %v", err)
				} else {
					u.azureClient = azureClient
				}
			}
		}
	}

	switch mode {
	case CleanupModeLocal:
		return u.cleanupLocal(ctx)
	case CleanupModeFull:
		return u.cleanupFull(ctx)
	default:
		return fmt.Errorf("invalid cleanup mode: %s", mode)
	}
}

// isPrivateCluster checks if the config indicates a private cluster.
func (u *Uninstaller) isPrivateCluster() bool {
	return u.config != nil &&
		u.config.Azure.TargetCluster != nil &&
		u.config.Azure.TargetCluster.IsPrivateCluster
}

// cleanupLocal performs local cleanup (keeps Gateway).
func (u *Uninstaller) cleanupLocal(ctx context.Context) error {
	u.logger.Infof("Performing local cleanup (keeping Gateway)...")

	hostname, err := GetHostname()
	if err != nil {
		return err
	}

	u.readVPNConfig()
	u.removeNodeFromCluster(ctx, hostname) // Must happen while VPN is still up
	u.removeClientPeerFromGateway(ctx)
	u.stopVPN(ctx)
	u.deleteVPNConfig()
	u.cleanupHostsEntries()

	u.logger.Infof("Local cleanup completed!")
	u.logger.Infof("To rejoin cluster, run:")
	u.logger.Infof("  sudo -E ./aks-flex-node agent --config config.json  # with private: true")

	return nil
}

// cleanupFull performs full cleanup (removes all Azure resources).
func (u *Uninstaller) cleanupFull(ctx context.Context) error {
	u.logger.Infof("Performing full cleanup...")

	hostname, err := GetHostname()
	if err != nil {
		return err
	}

	u.readVPNConfig()
	u.removeNodeFromCluster(ctx, hostname) // Must happen while VPN is still up
	u.removeClientPeerFromGateway(ctx)
	u.stopVPN(ctx)
	u.deleteVPNConfig()
	u.cleanupHostsEntries()

	if err := u.deleteAzureResources(ctx); err != nil {
		u.logger.Warnf("Failed to delete some Azure resources: %v", err)
	}

	u.deleteSSHKeys()

	u.logger.Infof("Full cleanup completed!")
	u.logger.Infof("All components and Azure resources have been removed.")
	u.logger.Infof("The local machine is now clean.")

	return nil
}

// readVPNConfig reads Gateway IP and client key from VPN config.
func (u *Uninstaller) readVPNConfig() {
	vpnClient := NewVPNClient(u.vpnConfig, u.logger)
	gatewayIP, clientKey, err := vpnClient.GetClientConfigInfo()
	if err == nil {
		u.gatewayIP = gatewayIP
		u.clientKey = clientKey
	}
}

// removeNodeFromCluster removes the node from the Kubernetes cluster.
func (u *Uninstaller) removeNodeFromCluster(ctx context.Context, nodeName string) {
	if !CommandExists("kubectl") {
		return
	}

	u.logger.Infof("Removing node %s from cluster...", nodeName)

	if _, err := utils.RunCommandWithOutputContext(ctx, "kubectl", "--kubeconfig", "/root/.kube/config",
		"delete", "node", nodeName, "--ignore-not-found"); err == nil {
		u.logger.Infof("Node removed from cluster")
		return
	}

	if _, err := utils.RunCommandWithOutputContext(ctx, "kubectl", "delete", "node", nodeName, "--ignore-not-found"); err == nil {
		u.logger.Infof("Node removed from cluster")
		return
	}

	u.logger.Warnf("Failed to remove node from cluster (may need manual cleanup: kubectl delete node %s)", nodeName)
}

// removeClientPeerFromGateway removes this client's peer from the Gateway.
func (u *Uninstaller) removeClientPeerFromGateway(ctx context.Context) {
	if u.gatewayIP == "" || u.clientKey == "" || !utils.FileExists(u.sshKeyPath) {
		return
	}

	u.logger.Infof("Removing client peer from Gateway...")

	vpnClient := NewVPNClient(u.vpnConfig, u.logger)
	clientPubKey, err := vpnClient.GetPublicKeyFromPrivate(ctx, u.clientKey)
	if err != nil || clientPubKey == "" {
		return
	}

	sshConfig := DefaultSSHConfig(u.sshKeyPath, u.gatewayIP)
	sshConfig.Timeout = 10
	ssh := NewSSHClient(sshConfig, u.logger)
	vpnServer := NewVPNServerManager(ssh, u.logger)

	_ = vpnServer.RemovePeer(ctx, clientPubKey)
	u.logger.Infof("Client peer removed from Gateway")
}

// stopVPN stops the VPN connection.
func (u *Uninstaller) stopVPN(ctx context.Context) {
	vpnClient := NewVPNClient(u.vpnConfig, u.logger)
	_ = vpnClient.Stop(ctx)
	u.logger.Infof("VPN connection stopped")
}

// deleteVPNConfig deletes the VPN client configuration.
func (u *Uninstaller) deleteVPNConfig() {
	vpnClient := NewVPNClient(u.vpnConfig, u.logger)
	_ = vpnClient.RemoveClientConfig()
	u.logger.Infof("VPN config deleted")
}

// cleanupHostsEntries removes AKS-related entries from /etc/hosts.
func (u *Uninstaller) cleanupHostsEntries() {
	_ = RemoveHostsEntries("privatelink")
	_ = RemoveHostsEntries("azmk8s.io")
	u.logger.Infof("Hosts entries cleaned")
}

// deleteSSHKeys deletes the Gateway SSH keys.
func (u *Uninstaller) deleteSSHKeys() {
	_ = RemoveSSHKeys(u.sshKeyPath)
	u.logger.Infof("SSH keys deleted")
}

// deleteAzureResources deletes all Azure resources created for the Gateway.
func (u *Uninstaller) deleteAzureResources(ctx context.Context) error {
	if u.clusterInfo == nil || u.azureClient == nil {
		return fmt.Errorf("cluster info or Azure client not available")
	}

	u.logger.Infof("Deleting Azure resources...")

	gatewayName := "wg-gateway"
	pipName := gatewayName + "-pip"
	nsgName := gatewayName + "-nsg"

	// VM deletion cascades to NIC and OS disk via DeleteOption set at creation time.
	if err := u.azureClient.DeleteVM(ctx, u.clusterInfo.ResourceGroup, gatewayName); err != nil {
		u.logger.Warnf("Delete VM: %v", err)
	}
	if err := u.azureClient.DeletePublicIP(ctx, u.clusterInfo.ResourceGroup, pipName); err != nil {
		u.logger.Warnf("Delete Public IP: %v", err)
	}
	if err := u.azureClient.DeleteNSG(ctx, u.clusterInfo.ResourceGroup, nsgName); err != nil {
		u.logger.Warnf("Delete NSG: %v", err)
	}

	clusterInfo, err := u.azureClient.GetPrivateClusterInfo(ctx, u.clusterInfo.ResourceGroup, u.clusterInfo.ClusterName)
	if err == nil && clusterInfo.VNetName != "" {
		_ = u.azureClient.DeleteSubnet(ctx, clusterInfo.VNetResourceGroup, clusterInfo.VNetName, "wg-subnet")
	}
	u.logger.Infof("Azure resources deleted")

	return nil
}
