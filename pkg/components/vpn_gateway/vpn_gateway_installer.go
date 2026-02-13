package vpn_gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v4"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/apparentlymart/go-cidr/cidr"
	"github.com/sirupsen/logrus"

	"go.goms.io/aks/AKSFlexNode/pkg/auth"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
)

// Installer handles VPN Gateway installation operations
type Installer struct {
	config         *config.Config
	logger         *logrus.Logger
	vnetClient     *armnetwork.VirtualNetworksClient
	subnetsClient  *armnetwork.SubnetsClient
	vgwClient      *armnetwork.VirtualNetworkGatewaysClient
	publicIPClient *armnetwork.PublicIPAddressesClient
}

// NewInstaller creates a new VPN Gateway installer
func NewInstaller(logger *logrus.Logger) *Installer {
	return &Installer{
		config: config.GetConfig(),
		logger: logger,
	}
}

// Validate validates prerequisites for VPN Gateway installation
func (i *Installer) Validate(ctx context.Context) error {
	if !i.config.IsVPNGatewayEnabled() {
		i.logger.Info("VPN Gateway setup is not enabled in configuration, skipping Validate...")
		return nil
	}

	if i.config.Azure.VPNGateway.P2SGatewayCIDR == "" {
		return fmt.Errorf("P2S Gateway CIDR is not configured")
	}

	if i.config.Azure.VPNGateway.PodCIDR == "" {
		return fmt.Errorf("pod CIDR is not configured - this is required for VPN network routing")
	}

	if i.config.Azure.VPNGateway.VNetID == "" {
		return fmt.Errorf("VNet ID for VPN Gateway is not configured")
	}

	// Validate that VNet ID is a proper Azure resource ID
	if err := utils.ValidateAzureResourceID(i.config.Azure.VPNGateway.VNetID, "virtualNetworks"); err != nil {
		return fmt.Errorf("invalid VNet ID: %w", err)
	}

	return nil
}

// GetName returns the step name
func (i *Installer) GetName() string {
	return "VPNGatewayInstaller"
}

type vnetResourceInfo struct {
	vnetID            string
	location          string
	resourceGroupName string
	subscriptionID    string
	vnet              *armnetwork.VirtualNetwork
}

// Execute performs VPN Gateway setup as part of the bootstrap process
// This method handles the whole VPN Gateway creation and configuration flow:
// 1. VPN Gateway provisioning
// 2. Certificate generation and upload
// 3. VPN client configuration download
// 4. VPN connection establishment
func (i *Installer) Execute(ctx context.Context) error {
	i.logger.Info("Starting VPN Gateway setup for bootstrap process")

	// Set up Azure clients
	if err := i.setUpClients(ctx); err != nil {
		i.logger.Errorf("Failed to set up Azure clients: %v", err)
		return fmt.Errorf("vpn gateway setup failed at client setup: %w", err)
	}

	// Discover the VNet used by AKS cluster nodes - it can be either BYO VNet or AKS managed VNet
	// The VPN Gateway will be created in this VNet to establish connectivity between the flex node and AKS cluster nodes
	vnetInfo, err := i.getNodeVNet(ctx)
	if err != nil {
		i.logger.Errorf("Failed to get AKS managed VNet: %v", err)
		return fmt.Errorf("vpn gateway setup failed at VNet discovery: %w", err)
	}

	// Provision VPN Gateway in the AKS Node VNet
	_, err = i.provisionGateway(ctx, vnetInfo)
	if err != nil {
		i.logger.Errorf("Failed to provision VPN Gateway: %v", err)
		return fmt.Errorf("vpn gateway setup failed at gateway provisioning: %w", err)
	}

	// Check if VPN connection is already working before setting up certificates
	if i.isVPNConnected() {
		i.logger.Info("VPN connection is already established, skipping certificate setup and connection establishment")
	} else {
		// Setup VPN Gateway certificates (root and client)
		i.logger.Info("Setting up VPN certificates")
		if err := i.setupCertificates(ctx, vnetInfo); err != nil {
			i.logger.Errorf("Failed to setup certificates: %v", err)
			return fmt.Errorf("vpn gateway setup failed at certificate setup: %w", err)
		}
		i.logger.Info("VPN certificates setup completed")

		// Download VPN configuration
		i.logger.Info("Downloading VPN client configuration")
		configPath, err := i.downloadVPNConfig(ctx, vnetInfo)
		if err != nil {
			i.logger.Errorf("Failed to download VPN configuration: %v", err)
			return fmt.Errorf("vpn gateway setup failed at config download: %w", err)
		}
		i.logger.Infof("VPN configuration downloaded to: %s", configPath)

		// Establish VPN connection using the downloaded configuration
		i.logger.Info("Establishing VPN connection")
		connected, err := i.establishVPNConnection(ctx, configPath)
		if err != nil {
			i.logger.Errorf("Failed to establish VPN connection: %v", err)
			return fmt.Errorf("vpn gateway setup failed at connection establishment: %w", err)
		}
		if !connected {
			return fmt.Errorf("vpn gateway setup failed: VPN connection could not be established")
		}
		i.logger.Info("VPN connection established successfully")
	}

	// Always configure network routes and iptables rules
	i.logger.Info("Configuring VPN network routing")
	if err := i.configureVPNNetworking(ctx, vnetInfo); err != nil {
		i.logger.Errorf("Failed to configure VPN networking: %v", err)
		return fmt.Errorf("vpn gateway setup failed at network configuration: %w", err)
	}
	i.logger.Info("VPN networking configuration completed")

	i.logger.Info("VPN Gateway setup completed successfully")
	return nil
}

// setUpAKSClients sets up Azure Container Service clients using the target cluster subscription ID
func (i *Installer) setUpClients(ctx context.Context) error {
	cred, err := auth.NewAuthProvider().UserCredential(config.GetConfig())
	if err != nil {
		return fmt.Errorf("failed to get authentication credential: %w", err)
	}

	vnetID := i.config.GetVPNGatewayVNetID()
	if vnetID == "" {
		return fmt.Errorf("failed to get VNet ID from configuration")
	}
	vnetSub := utils.GetSubscriptionIDFromResourceID(vnetID)

	clientFactory, err := armnetwork.NewClientFactory(vnetSub, cred, nil)
	if err != nil {
		return fmt.Errorf("failed to create Azure Network client factory: %w", err)
	}

	i.vnetClient = clientFactory.NewVirtualNetworksClient()
	i.subnetsClient = clientFactory.NewSubnetsClient()
	i.vgwClient = clientFactory.NewVirtualNetworkGatewaysClient()
	i.publicIPClient = clientFactory.NewPublicIPAddressesClient()
	return nil
}

// IsCompleted checks if VPN Gateway setup has been completed
func (i *Installer) IsCompleted(ctx context.Context) bool {
	if !i.config.IsVPNGatewayEnabled() {
		i.logger.Info("VPN Gateway setup is disabled in configuration, skipping installation...")
		return true
	}
	return false
}

// provisionGateway handles VPN Gateway provisioning with idempotency
func (i *Installer) provisionGateway(ctx context.Context, vnetInfo vnetResourceInfo) (*armnetwork.VirtualNetworkGateway, error) {
	// Check if VPN Gateway already exists
	if gateway, err := i.getVPNGateway(ctx, vnetInfo); err == nil && gateway != nil {
		i.logger.Infof("VPN Gateway already exists: %s", to.String(gateway.Name))
		return gateway, nil
	}

	i.logger.Infof("Provisioning VPN Gateway in VNet: %s", vnetInfo.vnetID)

	// Ensure GatewaySubnet exists
	if err := i.ensureGatewaySubnet(ctx, vnetInfo); err != nil {
		return nil, fmt.Errorf("failed to ensure gateway subnet: %w", err)
	}

	// Create Public IP for VPN Gateway
	publicIP, err := i.createPublicIPForVPNGateway(ctx, vnetInfo)
	if err != nil {
		return nil, fmt.Errorf("failed to create public IP: %w", err)
	}

	// Create VPN Gateway in the GatewaySubnet
	gateway, err := i.createVPNGateway(ctx, vnetInfo, publicIP)
	if err != nil {
		return nil, fmt.Errorf("failed to create VPN Gateway: %w", err)
	}

	i.logger.Infof("Successfully provisioned VPN Gateway: %s", to.String(gateway.Name))
	return gateway, nil
}

// createPublicIPForVPNGateway creates a public IP for the VPN Gateway
func (i *Installer) createPublicIPForVPNGateway(ctx context.Context, vnetInfo vnetResourceInfo) (string, error) {
	i.logger.Infof("Ensuring public IP exists: %s", gatewayPublicIPName)

	// Prepare Public IP parameters
	allocationMethod := armnetwork.IPAllocationMethodStatic
	skuName := armnetwork.PublicIPAddressSKUNameStandard
	skuTier := armnetwork.PublicIPAddressSKUTierRegional

	publicIPParams := armnetwork.PublicIPAddress{
		Location: &vnetInfo.location,
		SKU: &armnetwork.PublicIPAddressSKU{
			Name: &skuName,
			Tier: &skuTier,
		},
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: &allocationMethod,
		},
		Zones: []*string{
			&[]string{"1"}[0],
		},
	}

	// Create Public IP - this is a long-running operation
	poller, err := i.publicIPClient.BeginCreateOrUpdate(ctx, vnetInfo.resourceGroupName, gatewayPublicIPName, publicIPParams, nil)
	if err != nil {
		return "", fmt.Errorf("failed to start public IP creation: %w", err)
	}

	i.logger.Info("Public IP creation initiated. Waiting for completion...")

	// Wait for completion
	result, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create public IP: %w", err)
	}

	i.logger.Infof("Successfully created public IP: %s", to.String(result.ID))
	return to.String(result.ID), nil
}

// setupCertificates handles certificate generation and upload
func (i *Installer) setupCertificates(ctx context.Context, vnetInfo vnetResourceInfo) error {
	i.logger.Info("Setting up VPN root certificates...")
	certData, err := i.generateCertificates()
	if err != nil {
		return fmt.Errorf("failed to generate VPN certificates: %w", err)
	}

	i.logger.Info("Uploading VPN root certificate to Azure VPN Gateway...")
	if err := i.uploadCertificateToAzure(ctx, certData, vnetInfo); err != nil {
		i.logger.Warnf("Certificate upload failed: %v", err)
		return fmt.Errorf("failed to upload certificate to Azure: %w", err)
	}
	i.logger.Info("Certificate uploaded to Azure VPN Gateway successfully")

	return nil
}

// downloadVPNConfig downloads and saves the VPN configuration
func (i *Installer) downloadVPNConfig(ctx context.Context, vnetInfo vnetResourceInfo) (string, error) {
	i.logger.Info("Downloading VPN client configuration...")
	configData, err := i.downloadVPNClientConfig(ctx, defaultVPNGatewayName, vnetInfo.resourceGroupName)
	if err != nil {
		return "", fmt.Errorf("failed to download VPN client configuration: %w", err)
	}

	// Save configuration to file
	configPath, err := i.saveVPNConfig(configData)
	if err != nil {
		return "", fmt.Errorf("failed to save VPN config: %w", err)
	}

	return configPath, nil
}

// establishVPNConnection establishes the VPN connection
func (i *Installer) establishVPNConnection(ctx context.Context, configPath string) (bool, error) {
	i.logger.Info("Setting up OpenVPN with downloaded configuration...")
	if err := i.setupOpenVPN(configPath); err != nil {
		return false, fmt.Errorf("failed to setup OpenVPN: %w", err)
	}

	i.logger.Info("Waiting for VPN connection to establish...")
	if err := i.waitForVPNConnection(vpnConnectionTimeout); err != nil {
		return false, fmt.Errorf("VPN connection failed to establish: %w", err)
	}

	i.logger.Info("VPN connection established successfully")
	return true, nil
}

// waitForVPNConnection waits for VPN connection to be established
func (i *Installer) waitForVPNConnection(timeout time.Duration) error {
	i.logger.Infof("Waiting up to %v for VPN connection...", timeout)

	start := time.Now()
	for time.Since(start) < timeout {
		if i.isVPNConnected() {
			i.logger.Info("VPN connection established successfully")
			return nil
		}

		i.logger.Debug("VPN not connected yet, waiting...")
		time.Sleep(vpnConnectionCheckInterval)
	}

	return fmt.Errorf("VPN connection timeout after %v", timeout)
}

// saveVPNConfig saves VPN configuration to the appropriate directory
func (i *Installer) saveVPNConfig(configData string) (string, error) {
	configPath := GetVPNConfigPath()

	// Save VPN config to the persistent location atomically
	if err := utils.WriteFileAtomicSystem(configPath, []byte(configData), certificateFilePerm); err != nil {
		return "", fmt.Errorf("failed to save VPN config file: %w", err)
	}

	i.logger.Infof("VPN configuration saved to: %s", configPath)
	return configPath, nil
}

// calculateGatewaySubnetCIDR calculates an appropriate GatewaySubnet CIDR
func (i *Installer) calculateGatewaySubnetCIDR(ctx context.Context, vnetInfo vnetResourceInfo) (string, error) {
	i.logger.Infof("Calculating GatewaySubnet CIDR for VNet: %s", vnetInfo.vnetID)

	// proactive checks, should not happen
	if vnetInfo.vnet.Properties == nil ||
		vnetInfo.vnet.Properties.AddressSpace == nil ||
		len(vnetInfo.vnet.Properties.AddressSpace.AddressPrefixes) == 0 {
		return "", fmt.Errorf("VNet has no address prefixes")
	}

	// Try each address prefix until we find one with available space
	var lastErr error
	for idx, prefix := range vnetInfo.vnet.Properties.AddressSpace.AddressPrefixes {
		if prefix == nil {
			continue
		}

		vnetCIDR := *prefix
		i.logger.Infof("Trying VNet address prefix %d: %s", idx+1, vnetCIDR)

		// Calculate an available /27 subnet for GatewaySubnet in this address prefix
		gatewaySubnetCIDR, err := i.calculateAvailableSubnetInRange(vnetCIDR, vnetInfo.vnet.Properties.Subnets, gatewaySubnetPrefix)
		if err != nil {
			i.logger.Warnf("No available space in address prefix %s: %v", vnetCIDR, err)
			lastErr = err
			continue
		}

		i.logger.Infof("Successfully calculated GatewaySubnet CIDR: %s in address prefix: %s", gatewaySubnetCIDR, vnetCIDR)
		return gatewaySubnetCIDR, nil
	}

	// If we get here, no address prefix had available space
	return "", fmt.Errorf("no available space for GatewaySubnet in any VNet address prefix. Last error: %w", lastErr)
}

// calculateAvailableSubnetInRange finds an available subnet within the VNet address space
func (i *Installer) calculateAvailableSubnetInRange(vnetCIDR string, existingSubnets []*armnetwork.Subnet, prefixLength int) (string, error) {
	// Parse VNet CIDR
	_, vnetNet, err := net.ParseCIDR(vnetCIDR)
	if err != nil {
		return "", fmt.Errorf("failed to parse VNet CIDR %s: %w", vnetCIDR, err)
	}

	// Only support IPv4
	if vnetNet.IP.To4() == nil {
		return "", fmt.Errorf("only IPv4 networks are supported")
	}

	// Validate prefix length
	if prefixLength < 0 || prefixLength > 32 {
		return "", fmt.Errorf("invalid prefix length: %d", prefixLength)
	}

	vnetPrefixLen, _ := vnetNet.Mask.Size()
	if prefixLength <= vnetPrefixLen {
		return "", fmt.Errorf("requested subnet prefix /%d must be larger than VNet prefix /%d", prefixLength, vnetPrefixLen)
	}

	// Convert existing subnets to IPNet for overlap checking
	var existingNets []*net.IPNet
	for _, subnet := range existingSubnets {
		if subnet.Properties.AddressPrefix != nil && *subnet.Properties.AddressPrefix != "" {
			_, subnetNet, err := net.ParseCIDR(*subnet.Properties.AddressPrefix)
			if err != nil {
				i.logger.Warnf("Failed to parse existing subnet CIDR %s: %v", *subnet.Properties.AddressPrefix, err)
				continue
			}
			existingNets = append(existingNets, subnetNet)
		}
	}

	// Use go-cidr library to generate all possible subnets of the desired size
	subnetBits := prefixLength - vnetPrefixLen

	// Find a subnet that doesn't overlap with existing ones, starting from the end
	// We'll iterate through possible subnet positions
	maxSubnets := 1 << subnetBits
	for idx := maxSubnets - 1; idx >= 0; idx-- {
		candidateNet, err := cidr.Subnet(vnetNet, subnetBits, idx)
		if err != nil {
			continue
		}

		// Check if this subnet overlaps with any existing subnet
		overlaps := false
		for _, existing := range existingNets {
			if i.subnetsOverlap(candidateNet, existing) {
				overlaps = true
				break
			}
		}

		if !overlaps {
			return candidateNet.String(), nil
		}
	}

	return "", fmt.Errorf("no available /%d subnet found in VNet %s", prefixLength, vnetCIDR)
}

// subnetsOverlap checks if two subnets overlap
func (i *Installer) subnetsOverlap(subnet1, subnet2 *net.IPNet) bool {
	// A simpler and more reliable approach using standard Contains checks
	// Two subnets overlap if either contains the other's network or broadcast address
	firstIP1, lastIP1 := cidr.AddressRange(subnet1)
	firstIP2, lastIP2 := cidr.AddressRange(subnet2)

	// Check if any boundary IP of one subnet is contained in the other
	return subnet1.Contains(firstIP2) || subnet1.Contains(lastIP2) ||
		   subnet2.Contains(firstIP1) || subnet2.Contains(lastIP1)
}

// AKS nodes can be in either BYO VNet or AKS managed VNet
func (i *Installer) getNodeVNet(ctx context.Context) (vnetResourceInfo, error) {
	// First try to discover BYO VNet from agent pools
	vnetID := i.config.GetVPNGatewayVNetID()
	// Get VNet details
	vnetResp, err := i.vnetClient.Get(ctx,
		utils.GetResourceGroupFromResourceID(vnetID),
		utils.GetResourceNameFromResourceID(vnetID), nil)
	if err != nil {
		return vnetResourceInfo{}, fmt.Errorf("failed to get VNet details for VNet ID %s: %w", vnetID, err)
	}

	vnet := &vnetResp.VirtualNetwork
	vnetInfo := vnetResourceInfo{
		vnetID:            to.String(vnet.ID),
		location:          to.String(vnet.Location),
		resourceGroupName: utils.GetResourceGroupFromResourceID(to.String(vnet.ID)),
		subscriptionID:    utils.GetSubscriptionIDFromResourceID(to.String(vnet.ID)),
		vnet:              vnet,
	}

	return vnetInfo, nil
}

// getVPNGateway finds a VPN Gateway by name using Azure SDK
func (i *Installer) getVPNGateway(ctx context.Context, vnetInfo vnetResourceInfo) (*armnetwork.VirtualNetworkGateway, error) {
	// Get the specific VPN Gateway by name
	resp, err := i.vgwClient.Get(ctx, vnetInfo.resourceGroupName, defaultVPNGatewayName, nil)
	if err != nil {
		if strings.Contains(err.Error(), "NotFound") {
			i.logger.Infof("VPN Gateway '%s' not found in resource group '%s'", defaultVPNGatewayName, vnetInfo.resourceGroupName)
			return nil, errors.New("NotFound") // VPN Gateway not found
		}
		return nil, fmt.Errorf("failed to get VPN Gateway '%s' in resource group '%s': %w", defaultVPNGatewayName, vnetInfo.resourceGroupName, err)
	}

	// Verify it's a VPN Gateway (GatewayType == "Vpn")
	if resp.Properties != nil &&
		resp.Properties.GatewayType != nil &&
		*resp.Properties.GatewayType == armnetwork.VirtualNetworkGatewayTypeVPN {

		i.logger.Infof("Found VPN Gateway '%s' with GatewayType 'Vpn' in resource group '%s'", defaultVPNGatewayName, vnetInfo.resourceGroupName)
		return &resp.VirtualNetworkGateway, nil
	}

	i.logger.Infof("Gateway '%s' found but is not a VPN Gateway (GatewayType: %v)", defaultVPNGatewayName, resp.Properties.GatewayType)
	return nil, errors.New("NotFound") // Gateway exists but is not a VPN Gateway
}

// createVPNGateway creates a VPN Gateway
func (i *Installer) createVPNGateway(ctx context.Context, vnetInfo vnetResourceInfo, publicIPID string) (*armnetwork.VirtualNetworkGateway, error) {
	i.logger.Infof("Creating VPN Gateway: %s in resource group: %s", vpnGatewayName, vnetInfo.resourceGroupName)

	// Construct gateway subnet ID
	gatewaySubnetID := fmt.Sprintf("%s/subnets/%s", vnetInfo.vnetID, gatewaySubnetName)

	// Prepare VPN Gateway configuration
	vpnGwSKU := armnetwork.VirtualNetworkGatewaySKUNameVPNGw2AZ
	vpnGwTier := armnetwork.VirtualNetworkGatewaySKUTierVPNGw2AZ
	gatewayType := armnetwork.VirtualNetworkGatewayTypeVPN
	vpnType := armnetwork.VPNTypeRouteBased
	enableBgp := false
	activeActive := false

	// IP Configuration name
	ipConfigName := p2sConfigName

	// VPN Client Configuration
	p2sGatewayCIDR := i.config.Azure.VPNGateway.P2SGatewayCIDR
	vpnClientProtocol := armnetwork.VPNClientProtocolOpenVPN

	gatewayParams := armnetwork.VirtualNetworkGateway{
		Location: &vnetInfo.location,
		Properties: &armnetwork.VirtualNetworkGatewayPropertiesFormat{
			SKU: &armnetwork.VirtualNetworkGatewaySKU{
				Name: &vpnGwSKU,
				Tier: &vpnGwTier,
			},
			GatewayType: &gatewayType,
			VPNType:     &vpnType,
			EnableBgp:   &enableBgp,
			Active:      &activeActive,
			IPConfigurations: []*armnetwork.VirtualNetworkGatewayIPConfiguration{
				{
					Name: &ipConfigName,
					Properties: &armnetwork.VirtualNetworkGatewayIPConfigurationPropertiesFormat{
						PublicIPAddress: &armnetwork.SubResource{
							ID: &publicIPID,
						},
						Subnet: &armnetwork.SubResource{
							ID: &gatewaySubnetID,
						},
					},
				},
			},
			VPNClientConfiguration: &armnetwork.VPNClientConfiguration{
				VPNClientAddressPool: &armnetwork.AddressSpace{
					AddressPrefixes: []*string{&p2sGatewayCIDR},
				},
				VPNClientProtocols: []*armnetwork.VPNClientProtocol{&vpnClientProtocol},
			},
		},
	}

	// Create VPN Gateway - this is a long-running operation
	poller, err := i.vgwClient.BeginCreateOrUpdate(ctx, vnetInfo.resourceGroupName, vpnGatewayName, gatewayParams, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to start VPN Gateway creation: %w", err)
	}

	i.logger.Info("VPN Gateway creation initiated. Waiting for completion (this may take 20-30 minutes)...")

	resp, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create VPN Gateway: %w", err)
	}

	i.logger.Infof("VPN Gateway creation completed: %s", *resp.Name)
	return &resp.VirtualNetworkGateway, nil
}

// ensureGatewaySubnet creates GatewaySubnet if it doesn't exist
func (i *Installer) ensureGatewaySubnet(ctx context.Context, vnetInfo vnetResourceInfo) error {
	// Check if GatewaySubnet already exists
	for _, subnet := range vnetInfo.vnet.Properties.Subnets {
		if strings.EqualFold(to.String(subnet.Name), gatewaySubnetName) {
			i.logger.Infof("GatewaySubnet already exists in VNet %s", vnetInfo.vnetID)
			return nil
		}
	}

	// Calculate a CIDR for GatewaySubnet to ensure no
	gatewaySubnetCIDR, err := i.calculateGatewaySubnetCIDR(ctx, vnetInfo)
	if err != nil {
		return fmt.Errorf("failed to calculate gateway subnet CIDR: %w", err)
	}

	i.logger.Infof("Creating GatewaySubnet with CIDR: %s", gatewaySubnetCIDR)

	gatewaySubnetParams := armnetwork.Subnet{
		Properties: &armnetwork.SubnetPropertiesFormat{
			AddressPrefix: &gatewaySubnetCIDR,
		},
	}

	// Create the subnet - this is a long-running operation
	poller, err := i.subnetsClient.BeginCreateOrUpdate(ctx, vnetInfo.resourceGroupName, to.String(vnetInfo.vnet.Name), gatewaySubnetName, gatewaySubnetParams, nil)
	if err != nil {
		return fmt.Errorf("failed to start GatewaySubnet creation: %w", err)
	}

	i.logger.Info("GatewaySubnet creation initiated. Waiting for completion...")

	// Wait for completion
	result, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to create GatewaySubnet: %w", err)
	}

	i.logger.Infof("Successfully created GatewaySubnet: %s", *result.Name)
	return nil
}
