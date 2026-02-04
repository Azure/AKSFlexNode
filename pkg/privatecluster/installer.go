package privatecluster

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/sirupsen/logrus"

	"go.goms.io/aks/AKSFlexNode/pkg/auth"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
)

// Installer handles private cluster VPN/Gateway setup, implementing bootstrapper.StepExecutor.
type Installer struct {
	config        *config.Config
	logger        *logrus.Logger
	authProvider  *auth.AuthProvider
	azureClient   *AzureClient
	toolInstaller *ToolInstaller

	clusterInfo *AKSClusterInfo
	vpnConfig   VPNConfig
	sshKeyPath  string
	gatewayIP   string
}

// NewInstaller creates a new private cluster Installer.
func NewInstaller(logger *logrus.Logger) *Installer {
	return &Installer{
		config:        config.GetConfig(),
		logger:        logger,
		authProvider:  auth.NewAuthProvider(),
		toolInstaller: NewToolInstaller(logger),
		vpnConfig:     DefaultVPNConfig(),
		sshKeyPath:    GetSSHKeyPath(),
	}
}

// GetName returns the step name.
func (i *Installer) GetName() string {
	return "PrivateClusterInstall"
}

// Validate checks prerequisites for private cluster installation.
func (i *Installer) Validate(ctx context.Context) error {
	if !i.isPrivateCluster() {
		return nil
	}
	if os.Getuid() != 0 {
		return fmt.Errorf("private cluster setup requires root privileges, please run with 'sudo'")
	}
	return nil
}

// IsCompleted returns true for non-private clusters or when VPN is already connected.
func (i *Installer) IsCompleted(ctx context.Context) bool {
	if !i.isPrivateCluster() {
		return true
	}
	vpnClient := NewVPNClient(i.vpnConfig, i.logger)
	return vpnClient.TestConnection(ctx)
}

// Execute runs the private cluster installation (Gateway/VPN setup).
func (i *Installer) Execute(ctx context.Context) error {
	if !i.isPrivateCluster() {
		return nil
	}

	i.logger.Infof("========================================")
	i.logger.Infof(" Add Edge Node to Private AKS Cluster")
	i.logger.Infof("========================================")

	cred, err := i.authProvider.UserCredential(i.config)
	if err != nil {
		return fmt.Errorf("failed to get Azure credential: %w", err)
	}

	subscriptionID := i.config.GetTargetClusterSubscriptionID()
	azureClient, err := NewAzureClient(cred, subscriptionID, i.logger)
	if err != nil {
		return fmt.Errorf("failed to create Azure client: %w", err)
	}
	i.azureClient = azureClient

	i.clusterInfo = &AKSClusterInfo{
		ResourceID:     i.config.GetTargetClusterID(),
		SubscriptionID: subscriptionID,
		ResourceGroup:  i.config.GetTargetClusterResourceGroup(),
		ClusterName:    i.config.GetTargetClusterName(),
	}

	if err := i.checkEnvironment(ctx); err != nil {
		return fmt.Errorf("environment check failed: %w", err)
	}
	if err := i.setupGateway(ctx); err != nil {
		return fmt.Errorf("gateway setup failed: %w", err)
	}
	if err := i.setupVPNClient(ctx); err != nil {
		return fmt.Errorf("client setup failed: %w", err)
	}
	if err := i.joinNode(ctx); err != nil {
		return fmt.Errorf("node join failed: %w", err)
	}

	i.logger.Infof("Private cluster setup completed. Bootstrap will continue...")
	return nil
}

// isPrivateCluster checks if the config indicates a private cluster.
func (i *Installer) isPrivateCluster() bool {
	return i.config != nil &&
		i.config.Azure.TargetCluster != nil &&
		i.config.Azure.TargetCluster.IsPrivateCluster
}

// gatewayConfig returns the Gateway configuration, applying any overrides from config.
func (i *Installer) gatewayConfig() GatewayConfig {
	gw := DefaultGatewayConfig()
	if i.config.Azure.TargetCluster.GatewayVMSize != "" {
		gw.VMSize = i.config.Azure.TargetCluster.GatewayVMSize
	}
	if i.config.Azure.TargetCluster.GatewayPort > 0 {
		gw.Port = i.config.Azure.TargetCluster.GatewayPort
	}
	return gw
}

// checkEnvironment checks prerequisites for private cluster setup.
func (i *Installer) checkEnvironment(ctx context.Context) error {
	_ = CleanKubeCache()
	i.logger.Infof("Azure SDK client ready")
	i.logger.Infof("Subscription: %s", i.clusterInfo.SubscriptionID)

	tenantID, err := i.azureClient.GetTenantID(ctx)
	if err != nil {
		return err
	}
	i.clusterInfo.TenantID = tenantID
	i.logger.Debugf("Tenant ID: %s", tenantID)

	if !i.azureClient.AKSClusterExists(ctx, i.clusterInfo.ResourceGroup, i.clusterInfo.ClusterName) {
		return fmt.Errorf("AKS cluster '%s' not found", i.clusterInfo.ClusterName)
	}
	clusterInfo, err := i.azureClient.GetPrivateClusterInfo(ctx, i.clusterInfo.ResourceGroup, i.clusterInfo.ClusterName)
	if err != nil {
		return err
	}
	i.clusterInfo.Location = clusterInfo.Location
	i.clusterInfo.NodeResourceGroup = clusterInfo.NodeResourceGroup
	i.clusterInfo.PrivateFQDN = clusterInfo.PrivateFQDN
	i.clusterInfo.VNetName = clusterInfo.VNetName
	i.clusterInfo.VNetResourceGroup = clusterInfo.VNetResourceGroup
	i.logger.Infof("AKS cluster: %s (AAD/RBAC enabled)", i.clusterInfo.ClusterName)
	i.logger.Infof("VNet: %s/%s", i.clusterInfo.VNetResourceGroup, i.clusterInfo.VNetName)

	if err := InstallVPNTools(ctx, i.logger); err != nil {
		return fmt.Errorf("failed to install VPN tools: %w", err)
	}
	if err := i.toolInstaller.InstallKubectl(ctx, i.config.GetKubernetesVersion()); err != nil {
		return fmt.Errorf("failed to install kubectl: %w", err)
	}
	if err := i.toolInstaller.InstallKubelogin(ctx); err != nil {
		return fmt.Errorf("failed to install kubelogin: %w", err)
	}
	i.logger.Infof("Dependencies ready")

	return nil
}

// setupGateway sets up the VPN Gateway.
func (i *Installer) setupGateway(ctx context.Context) error {
	gateway := i.gatewayConfig()
	gatewayExists := false
	if i.azureClient.VMExists(ctx, i.clusterInfo.ResourceGroup, gateway.Name) {
		gatewayExists = true
		ip, err := i.azureClient.GetVMPublicIP(ctx, i.clusterInfo.ResourceGroup, gateway.Name)
		if err != nil {
			return fmt.Errorf("failed to get Gateway public IP: %w", err)
		}
		i.gatewayIP = ip
		i.logger.Infof("Gateway exists: %s", i.gatewayIP)
	} else {
		i.logger.Infof("Creating Gateway...")
		if err := i.createGatewayInfrastructure(ctx); err != nil {
			return err
		}
	}

	if err := GenerateSSHKey(i.sshKeyPath); err != nil {
		return fmt.Errorf("failed to generate SSH key: %w", err)
	}
	if err := i.azureClient.AddSSHKeyToVM(ctx, i.clusterInfo.ResourceGroup, gateway.Name, i.sshKeyPath); err != nil {
		return fmt.Errorf("failed to add SSH key to Gateway: %w", err)
	}

	if err := i.waitForVMReady(ctx, gatewayExists); err != nil {
		return err
	}

	if err := i.configureVPNServer(ctx); err != nil {
		return err
	}

	return nil
}

// createGatewayInfrastructure creates Gateway VM and related resources.
func (i *Installer) createGatewayInfrastructure(ctx context.Context) error {
	gateway := i.gatewayConfig()
	nsgName := gateway.Name + "-nsg"
	pipName := gateway.Name + "-pip"
	location := i.clusterInfo.Location

	if err := i.azureClient.CreateSubnet(ctx, i.clusterInfo.VNetResourceGroup, i.clusterInfo.VNetName,
		gateway.SubnetName, gateway.SubnetPrefix); err != nil {
		return fmt.Errorf("failed to create subnet: %w", err)
	}
	if err := i.azureClient.CreateNSG(ctx, i.clusterInfo.ResourceGroup, nsgName, location, gateway.Port); err != nil {
		return fmt.Errorf("failed to create NSG: %w", err)
	}
	if err := i.azureClient.CreatePublicIP(ctx, i.clusterInfo.ResourceGroup, pipName, location); err != nil {
		return fmt.Errorf("failed to create public IP: %w", err)
	}
	if err := GenerateSSHKey(i.sshKeyPath); err != nil {
		return fmt.Errorf("failed to generate SSH key: %w", err)
	}
	if err := i.azureClient.CreateVM(ctx, i.clusterInfo.ResourceGroup, gateway.Name,
		location, i.clusterInfo.VNetResourceGroup, i.clusterInfo.VNetName,
		gateway.SubnetName, nsgName, pipName,
		i.sshKeyPath, gateway.VMSize); err != nil {
		return fmt.Errorf("failed to create Gateway VM: %w", err)
	}

	ip, err := i.azureClient.GetPublicIPAddress(ctx, i.clusterInfo.ResourceGroup, pipName)
	if err != nil {
		return fmt.Errorf("failed to get public IP address: %w", err)
	}
	i.gatewayIP = ip
	i.logger.Infof("Gateway created: %s", i.gatewayIP)

	i.logger.Infof("Waiting for VM to boot (120s)...")
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(120 * time.Second):
	}

	return nil
}

// waitForVMReady waits for SSH connectivity to Gateway.
func (i *Installer) waitForVMReady(ctx context.Context, gatewayExists bool) error {
	sshConfig := DefaultSSHConfig(i.sshKeyPath, i.gatewayIP)
	ssh := NewSSHClient(sshConfig, i.logger)

	if ssh.TestConnection(ctx) {
		i.logger.Infof("SSH ready")
		return nil
	}

	if gatewayExists {
		gateway := i.gatewayConfig()
		i.logger.Infof("Restarting VM...")
		_ = i.azureClient.RestartVM(ctx, i.clusterInfo.ResourceGroup, gateway.Name)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(120 * time.Second):
		}
	}

	if err := ssh.WaitForConnection(ctx, 18, 10*time.Second); err != nil {
		return fmt.Errorf("VM SSH connection timeout")
	}
	i.logger.Infof("SSH ready")
	return nil
}

// configureVPNServer configures VPN on the Gateway.
func (i *Installer) configureVPNServer(ctx context.Context) error {
	sshConfig := DefaultSSHConfig(i.sshKeyPath, i.gatewayIP)
	ssh := NewSSHClient(sshConfig, i.logger)
	vpnServer := NewVPNServerManager(ssh, i.logger)

	if !vpnServer.IsInstalled(ctx) {
		i.logger.Infof("Installing VPN on Gateway...")
		if err := vpnServer.Install(ctx); err != nil {
			return fmt.Errorf("failed to install VPN on Gateway: %w", err)
		}
	}

	serverPubKey, err := vpnServer.GetPublicKey(ctx)
	if err != nil {
		if err := vpnServer.Install(ctx); err != nil {
			return err
		}
		serverPubKey, err = vpnServer.GetPublicKey(ctx)
		if err != nil {
			return err
		}
	}
	i.vpnConfig.ServerPublicKey = serverPubKey
	i.vpnConfig.ServerEndpoint = i.gatewayIP

	peerCount, _ := vpnServer.GetPeerCount(ctx)
	i.vpnConfig.ClientVPNIP = fmt.Sprintf("172.16.0.%d", peerCount+2)
	i.logger.Infof("VPN server ready, client IP: %s", i.vpnConfig.ClientVPNIP)

	return nil
}

// setupVPNClient configures the local VPN client.
func (i *Installer) setupVPNClient(ctx context.Context) error {
	gateway := i.gatewayConfig()
	vpnClient := NewVPNClient(i.vpnConfig, i.logger)
	privateKey, publicKey, err := vpnClient.GenerateKeyPair(ctx)
	if err != nil {
		return err
	}
	if err := vpnClient.CreateClientConfig(privateKey, gateway.Port); err != nil {
		return err
	}

	sshConfig := DefaultSSHConfig(i.sshKeyPath, i.gatewayIP)
	ssh := NewSSHClient(sshConfig, i.logger)
	vpnServer := NewVPNServerManager(ssh, i.logger)
	if err := vpnServer.AddPeer(ctx, publicKey, i.vpnConfig.ClientVPNIP); err != nil {
		return err
	}

	if err := vpnClient.Start(ctx); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(3 * time.Second):
	}

	if !vpnClient.TestConnection(ctx) {
		return fmt.Errorf("VPN connection failed")
	}
	i.logger.Infof("VPN connected: %s", i.vpnConfig.GatewayVPNIP)

	return nil
}

// joinNode joins the node to the AKS cluster.
func (i *Installer) joinNode(ctx context.Context) error {
	sshConfig := DefaultSSHConfig(i.sshKeyPath, i.gatewayIP)
	ssh := NewSSHClient(sshConfig, i.logger)
	vpnServer := NewVPNServerManager(ssh, i.logger)

	apiServerIP, err := vpnServer.ResolveDNS(ctx, i.clusterInfo.PrivateFQDN)
	if err != nil {
		return err
	}
	i.clusterInfo.APIServerIP = apiServerIP
	if err := AddHostsEntry(apiServerIP, i.clusterInfo.PrivateFQDN); err != nil {
		return fmt.Errorf("failed to add hosts entry: %w", err)
	}
	i.logger.Infof("API Server: %s (%s)", i.clusterInfo.PrivateFQDN, apiServerIP)

	_, _ = utils.RunCommandWithOutputContext(ctx, "swapoff", "-a")

	kubeconfigPath := "/root/.kube/config"
	if err := i.azureClient.GetAKSCredentials(ctx, i.clusterInfo.ResourceGroup, i.clusterInfo.ClusterName, kubeconfigPath); err != nil {
		return fmt.Errorf("failed to get AKS credentials: %w", err)
	}
	if _, err := utils.RunCommandWithOutputContext(ctx, "kubelogin", "convert-kubeconfig", "-l", "azurecli", "--kubeconfig", kubeconfigPath); err != nil {
		return fmt.Errorf("failed to convert kubeconfig: %w", err)
	}
	i.logger.Infof("Kubeconfig ready: %s", kubeconfigPath)

	return nil
}
