package privatecluster

import (
	"context"
	"fmt"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
)

// Installer handles private cluster installation
type Installer struct {
	logger        *Logger
	azureClient   *AzureClient
	toolInstaller *ToolInstaller
	options       InstallOptions

	// State collected during installation
	clusterInfo *AKSClusterInfo
	vpnConfig   VPNConfig
	sshKeyPath  string
	gatewayIP   string
}

// NewInstaller creates a new Installer instance.
// cred is the Azure credential used for SDK calls.
func NewInstaller(options InstallOptions, cred azcore.TokenCredential) (*Installer, error) {
	logger := NewLogger(options.Verbose)

	// Apply defaults
	if options.Gateway.Name == "" {
		options.Gateway = DefaultGatewayConfig()
	}

	subscriptionID, _, _, err := ParseResourceID(options.AKSResourceID)
	if err != nil {
		return nil, fmt.Errorf("failed to parse resource ID: %w", err)
	}

	azureClient, err := NewAzureClient(cred, subscriptionID, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create Azure client: %w", err)
	}

	return &Installer{
		logger:        logger,
		azureClient:   azureClient,
		toolInstaller: NewToolInstaller(logger),
		options:       options,
		vpnConfig:     DefaultVPNConfig(),
		sshKeyPath:    GetSSHKeyPath(),
	}, nil
}

// Install runs the complete installation process
func (i *Installer) Install(ctx context.Context) error {
	fmt.Printf("%s========================================%s\n", colorGreen, colorReset)
	fmt.Printf("%s Add Edge Node to Private AKS Cluster%s\n", colorGreen, colorReset)
	fmt.Printf("%s========================================%s\n\n", colorGreen, colorReset)

	// Parse resource ID
	subscriptionID, resourceGroup, clusterName, err := ParseResourceID(i.options.AKSResourceID)
	if err != nil {
		return err
	}

	i.clusterInfo = &AKSClusterInfo{
		ResourceID:     i.options.AKSResourceID,
		SubscriptionID: subscriptionID,
		ResourceGroup:  resourceGroup,
		ClusterName:    clusterName,
	}

	// Phase 1: Environment Check
	if err := i.phase1EnvironmentCheck(ctx); err != nil {
		return fmt.Errorf("environment check failed: %w", err)
	}

	// Phase 2: Gateway Setup
	if err := i.phase2GatewaySetup(ctx); err != nil {
		return fmt.Errorf("gateway setup failed: %w", err)
	}

	// Phase 3: Client Configuration
	if err := i.phase3ClientSetup(ctx); err != nil {
		return fmt.Errorf("client setup failed: %w", err)
	}

	// Phase 4: Node Join Preparation
	if err := i.phase4NodeJoin(ctx); err != nil {
		return fmt.Errorf("node join failed: %w", err)
	}

	// Phase 5 (Verification) skipped - node needs bootstrap to become Ready
	i.logger.Success("Private cluster setup completed. Bootstrap will continue...")
	return nil
}

// phase1EnvironmentCheck checks prerequisites
func (i *Installer) phase1EnvironmentCheck(ctx context.Context) error {
	_ = CleanKubeCache()
	i.logger.Success("Azure SDK client ready")
	i.logger.Success("Subscription: %s", i.clusterInfo.SubscriptionID)

	// Get Tenant ID
	tenantID, err := i.azureClient.GetTenantID(ctx)
	if err != nil {
		return err
	}
	i.clusterInfo.TenantID = tenantID
	i.logger.Verbose("Tenant ID: %s", tenantID)

	if !i.azureClient.AKSClusterExists(ctx, i.clusterInfo.ResourceGroup, i.clusterInfo.ClusterName) {
		return fmt.Errorf("AKS cluster '%s' not found", i.clusterInfo.ClusterName)
	}
	clusterInfo, err := i.azureClient.GetAKSClusterInfo(ctx, i.clusterInfo.ResourceGroup, i.clusterInfo.ClusterName)
	if err != nil {
		return err
	}
	i.clusterInfo.Location = clusterInfo.Location
	i.clusterInfo.NodeResourceGroup = clusterInfo.NodeResourceGroup
	i.clusterInfo.PrivateFQDN = clusterInfo.PrivateFQDN
	i.logger.Success("AKS cluster: %s (AAD/RBAC enabled)", i.clusterInfo.ClusterName)

	vnetName, vnetRG, err := i.azureClient.GetVNetInfo(ctx, i.clusterInfo.NodeResourceGroup)
	if err != nil {
		return err
	}
	i.clusterInfo.VNetName = vnetName
	i.clusterInfo.VNetResourceGroup = vnetRG
	i.logger.Success("VNet: %s/%s", vnetRG, vnetName)

	if err := InstallVPNTools(ctx, i.logger); err != nil {
		return fmt.Errorf("failed to install VPN tools: %w", err)
	}
	if err := InstallJQ(ctx, i.logger); err != nil {
		return fmt.Errorf("failed to install jq: %w", err)
	}
	if !CommandExists("kubectl") || !CommandExists("kubelogin") {
		if err := i.toolInstaller.InstallAKSCLI(ctx); err != nil {
			return fmt.Errorf("failed to install kubectl/kubelogin: %w", err)
		}
	}
	if !CommandExists("kubectl") {
		return fmt.Errorf("kubectl installation failed")
	}
	if !CommandExists("kubelogin") {
		return fmt.Errorf("kubelogin installation failed")
	}
	_ = i.toolInstaller.InstallConnectedMachineExtension(ctx)
	i.logger.Success("Dependencies ready")

	return nil
}

// phase2GatewaySetup sets up the VPN Gateway
func (i *Installer) phase2GatewaySetup(ctx context.Context) error {
	gatewayExists := false
	if i.azureClient.VMExists(ctx, i.clusterInfo.ResourceGroup, i.options.Gateway.Name) {
		gatewayExists = true
		ip, err := i.azureClient.GetVMPublicIP(ctx, i.clusterInfo.ResourceGroup, i.options.Gateway.Name)
		if err != nil {
			return fmt.Errorf("failed to get Gateway public IP: %w", err)
		}
		i.gatewayIP = ip
		i.logger.Success("Gateway exists: %s", i.gatewayIP)
	} else {
		i.logger.Info("Creating Gateway...")
		if err := i.createGatewayInfrastructure(ctx); err != nil {
			return err
		}
	}

	if err := GenerateSSHKey(i.sshKeyPath); err != nil {
		return fmt.Errorf("failed to generate SSH key: %w", err)
	}
	if err := i.azureClient.AddSSHKeyToVM(ctx, i.clusterInfo.ResourceGroup, i.options.Gateway.Name, i.sshKeyPath); err != nil {
		return fmt.Errorf("failed to add SSH key to Gateway: %w", err)
	}

	// Wait for VM ready
	if err := i.waitForVMReady(ctx, gatewayExists); err != nil {
		return err
	}

	// Get/configure server
	if err := i.configureVPNServer(ctx); err != nil {
		return err
	}

	return nil
}

// createGatewayInfrastructure creates Gateway VM and related resources
func (i *Installer) createGatewayInfrastructure(ctx context.Context) error {
	nsgName := i.options.Gateway.Name + "-nsg"
	pipName := i.options.Gateway.Name + "-pip"
	location := i.clusterInfo.Location

	if err := i.azureClient.CreateSubnet(ctx, i.clusterInfo.VNetResourceGroup, i.clusterInfo.VNetName,
		i.options.Gateway.SubnetName, i.options.Gateway.SubnetPrefix); err != nil {
		return fmt.Errorf("failed to create subnet: %w", err)
	}
	if err := i.azureClient.CreateNSG(ctx, i.clusterInfo.ResourceGroup, nsgName, location, i.options.Gateway.Port); err != nil {
		return fmt.Errorf("failed to create NSG: %w", err)
	}
	if err := i.azureClient.CreatePublicIP(ctx, i.clusterInfo.ResourceGroup, pipName, location); err != nil {
		return fmt.Errorf("failed to create public IP: %w", err)
	}
	if err := GenerateSSHKey(i.sshKeyPath); err != nil {
		return fmt.Errorf("failed to generate SSH key: %w", err)
	}
	if err := i.azureClient.CreateVM(ctx, i.clusterInfo.ResourceGroup, i.options.Gateway.Name,
		location, i.clusterInfo.VNetResourceGroup, i.clusterInfo.VNetName,
		i.options.Gateway.SubnetName, nsgName, pipName,
		i.sshKeyPath, i.options.Gateway.VMSize); err != nil {
		return fmt.Errorf("failed to create Gateway VM: %w", err)
	}

	ip, err := i.azureClient.GetPublicIPAddress(ctx, i.clusterInfo.ResourceGroup, pipName)
	if err != nil {
		return fmt.Errorf("failed to get public IP address: %w", err)
	}
	i.gatewayIP = ip
	i.logger.Success("Gateway created: %s", i.gatewayIP)

	i.logger.Info("Waiting for VM to boot (120s)...")
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(120 * time.Second):
	}

	return nil
}

// waitForVMReady waits for SSH connectivity to Gateway
func (i *Installer) waitForVMReady(ctx context.Context, gatewayExists bool) error {
	sshConfig := DefaultSSHConfig(i.sshKeyPath, i.gatewayIP)
	ssh := NewSSHClient(sshConfig, i.logger)

	if ssh.TestConnection(ctx) {
		i.logger.Success("SSH ready")
		return nil
	}

	if gatewayExists {
		i.logger.Info("Restarting VM...")
		_ = i.azureClient.RestartVM(ctx, i.clusterInfo.ResourceGroup, i.options.Gateway.Name)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(120 * time.Second):
		}
	}

	if err := ssh.WaitForConnection(ctx, 18, 10*time.Second); err != nil {
		return fmt.Errorf("VM SSH connection timeout")
	}
	i.logger.Success("SSH ready")
	return nil
}

// configureVPNServer configures VPN on the Gateway
func (i *Installer) configureVPNServer(ctx context.Context) error {
	sshConfig := DefaultSSHConfig(i.sshKeyPath, i.gatewayIP)
	ssh := NewSSHClient(sshConfig, i.logger)
	vpnServer := NewVPNServerManager(ssh, i.logger)

	if !vpnServer.IsInstalled(ctx) {
		i.logger.Info("Installing VPN on Gateway...")
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
	i.logger.Success("VPN server ready, client IP: %s", i.vpnConfig.ClientVPNIP)

	return nil
}

// phase3ClientSetup configures the local VPN client
func (i *Installer) phase3ClientSetup(ctx context.Context) error {
	vpnClient := NewVPNClient(i.vpnConfig, i.logger)
	privateKey, publicKey, err := vpnClient.GenerateKeyPair(ctx)
	if err != nil {
		return err
	}
	if err := vpnClient.CreateClientConfig(privateKey, i.options.Gateway.Port); err != nil {
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
	i.logger.Success("VPN connected: %s", i.vpnConfig.GatewayVPNIP)

	return nil
}

// phase4NodeJoin joins the node to the AKS cluster
func (i *Installer) phase4NodeJoin(ctx context.Context) error {
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
	i.logger.Success("API Server: %s (%s)", i.clusterInfo.PrivateFQDN, apiServerIP)

	_, _ = RunCommand(ctx, "swapoff", "-a")

	if !CommandExists("azcmagent") {
		if err := i.installArcAgent(ctx); err != nil {
			return fmt.Errorf("failed to install Arc agent: %w", err)
		}
	}

	kubeconfigPath := "/root/.kube/config"
	if err := i.azureClient.GetAKSCredentials(ctx, i.clusterInfo.ResourceGroup, i.clusterInfo.ClusterName, kubeconfigPath); err != nil {
		return fmt.Errorf("failed to get AKS credentials: %w", err)
	}
	if _, err := RunCommand(ctx, "kubelogin", "convert-kubeconfig", "-l", "azurecli", "--kubeconfig", kubeconfigPath); err != nil {
		return fmt.Errorf("failed to convert kubeconfig: %w", err)
	}
	i.logger.Success("Kubeconfig ready: %s", kubeconfigPath)

	return nil
}

// installArcAgent installs Azure Arc agent
func (i *Installer) installArcAgent(ctx context.Context) error {
	_, _ = RunCommand(ctx, "dpkg", "--purge", "azcmagent")
	if _, err := RunCommand(ctx, "curl", "-L", "-o", "/tmp/install_linux_azcmagent.sh",
		"https://gbl.his.arc.azure.com/azcmagent-linux"); err != nil {
		return err
	}
	if _, err := RunCommand(ctx, "chmod", "+x", "/tmp/install_linux_azcmagent.sh"); err != nil {
		return err
	}
	if _, err := RunCommand(ctx, "bash", "/tmp/install_linux_azcmagent.sh"); err != nil {
		return err
	}
	_, _ = RunCommand(ctx, "rm", "-f", "/tmp/install_linux_azcmagent.sh")
	return nil
}
