package privatecluster

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v5"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armsubscriptions"
	"github.com/sirupsen/logrus"
)

// AzureClient provides Azure operations using the Azure SDK for Go.
type AzureClient struct {
	logger         *logrus.Logger
	subscriptionID string

	vmClient           *armcompute.VirtualMachinesClient
	subnetClient       *armnetwork.SubnetsClient
	nsgClient          *armnetwork.SecurityGroupsClient
	pipClient          *armnetwork.PublicIPAddressesClient
	nicClient          *armnetwork.InterfacesClient
	aksClient          *armcontainerservice.ManagedClustersClient
	subscriptionClient *armsubscriptions.Client
}

// NewAzureClient creates a new AzureClient with all sub-clients initialized.
func NewAzureClient(cred azcore.TokenCredential, subscriptionID string, logger *logrus.Logger) (*AzureClient, error) {
	c := &AzureClient{
		logger:         logger,
		subscriptionID: subscriptionID,
	}

	var err error

	if c.vmClient, err = armcompute.NewVirtualMachinesClient(subscriptionID, cred, nil); err != nil {
		return nil, fmt.Errorf("failed to create VM client: %w", err)
	}
	if c.subnetClient, err = armnetwork.NewSubnetsClient(subscriptionID, cred, nil); err != nil {
		return nil, fmt.Errorf("failed to create subnet client: %w", err)
	}
	if c.nsgClient, err = armnetwork.NewSecurityGroupsClient(subscriptionID, cred, nil); err != nil {
		return nil, fmt.Errorf("failed to create NSG client: %w", err)
	}
	if c.pipClient, err = armnetwork.NewPublicIPAddressesClient(subscriptionID, cred, nil); err != nil {
		return nil, fmt.Errorf("failed to create public IP client: %w", err)
	}
	if c.nicClient, err = armnetwork.NewInterfacesClient(subscriptionID, cred, nil); err != nil {
		return nil, fmt.Errorf("failed to create NIC client: %w", err)
	}
	if c.aksClient, err = armcontainerservice.NewManagedClustersClient(subscriptionID, cred, nil); err != nil {
		return nil, fmt.Errorf("failed to create AKS client: %w", err)
	}
	if c.subscriptionClient, err = armsubscriptions.NewClient(cred, nil); err != nil {
		return nil, fmt.Errorf("failed to create subscription client: %w", err)
	}

	return c, nil
}

// GetTenantID returns the tenant ID for the configured subscription.
func (c *AzureClient) GetTenantID(ctx context.Context) (string, error) {
	resp, err := c.subscriptionClient.Get(ctx, c.subscriptionID, nil)
	if err != nil {
		return "", fmt.Errorf("failed to get subscription info: %w", err)
	}
	if resp.TenantID == nil {
		return "", fmt.Errorf("tenant ID not found for subscription %s", c.subscriptionID)
	}
	return *resp.TenantID, nil
}

// AKSClusterExists checks if an AKS cluster exists.
func (c *AzureClient) AKSClusterExists(ctx context.Context, resourceGroup, clusterName string) bool {
	_, err := c.aksClient.Get(ctx, resourceGroup, clusterName, nil)
	return err == nil
}

// GetPrivateClusterInfo retrieves cluster info needed for private cluster setup (location, node resource group, private FQDN)
// and validates that AAD and Azure RBAC are enabled.
func (c *AzureClient) GetPrivateClusterInfo(ctx context.Context, resourceGroup, clusterName string) (*AKSClusterInfo, error) {
	resp, err := c.aksClient.Get(ctx, resourceGroup, clusterName, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get AKS cluster: %w", err)
	}

	cluster := resp.ManagedCluster
	props := cluster.Properties
	if props == nil {
		return nil, fmt.Errorf("AKS cluster properties are nil")
	}

	if props.AADProfile == nil || props.AADProfile.Managed == nil || !*props.AADProfile.Managed {
		return nil, fmt.Errorf("AKS cluster AAD not enabled, please enable: az aks update --enable-aad")
	}

	if props.AADProfile.EnableAzureRBAC == nil || !*props.AADProfile.EnableAzureRBAC {
		return nil, fmt.Errorf("AKS cluster Azure RBAC not enabled, please enable: az aks update --enable-azure-rbac")
	}

	info := &AKSClusterInfo{
		ResourceGroup: resourceGroup,
		ClusterName:   clusterName,
	}

	if cluster.Location != nil {
		info.Location = *cluster.Location
	}
	if props.NodeResourceGroup != nil {
		info.NodeResourceGroup = *props.NodeResourceGroup
	}
	if props.PrivateFQDN != nil {
		info.PrivateFQDN = *props.PrivateFQDN
	}

	// Extract VNet info from agent pool subnet ID
	for _, pool := range props.AgentPoolProfiles {
		if pool.VnetSubnetID != nil && *pool.VnetSubnetID != "" {
			// Format: /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.Network/virtualNetworks/{vnet}/subnets/{subnet}
			parts := strings.Split(*pool.VnetSubnetID, "/")
			if len(parts) >= 9 {
				info.VNetResourceGroup = parts[4]
				info.VNetName = parts[8]
			}
			break
		}
	}

	return info, nil
}

// VMExists checks if a VM exists.
func (c *AzureClient) VMExists(ctx context.Context, resourceGroup, vmName string) bool {
	_, err := c.vmClient.Get(ctx, resourceGroup, vmName, nil)
	return err == nil
}

// GetVMPublicIP retrieves a VM's public IP address by tracing VM → NIC → PIP.
func (c *AzureClient) GetVMPublicIP(ctx context.Context, resourceGroup, vmName string) (string, error) {
	vmResp, err := c.vmClient.Get(ctx, resourceGroup, vmName, nil)
	if err != nil {
		return "", fmt.Errorf("failed to get VM: %w", err)
	}
	if vmResp.Properties == nil || vmResp.Properties.NetworkProfile == nil ||
		len(vmResp.Properties.NetworkProfile.NetworkInterfaces) == 0 {
		return "", fmt.Errorf("VM has no network interfaces")
	}

	nicID := vmResp.Properties.NetworkProfile.NetworkInterfaces[0].ID
	if nicID == nil {
		return "", fmt.Errorf("NIC ID is nil")
	}
	nicRG, nicName := parseResourceGroupAndName(*nicID)

	nicResp, err := c.nicClient.Get(ctx, nicRG, nicName, nil)
	if err != nil {
		return "", fmt.Errorf("failed to get NIC: %w", err)
	}
	if nicResp.Properties == nil || len(nicResp.Properties.IPConfigurations) == 0 {
		return "", fmt.Errorf("NIC has no IP configurations")
	}

	ipConfig := nicResp.Properties.IPConfigurations[0]
	if ipConfig.Properties == nil || ipConfig.Properties.PublicIPAddress == nil || ipConfig.Properties.PublicIPAddress.ID == nil {
		return "", fmt.Errorf("NIC has no public IP")
	}
	pipRG, pipName := parseResourceGroupAndName(*ipConfig.Properties.PublicIPAddress.ID)

	pipResp, err := c.pipClient.Get(ctx, pipRG, pipName, nil)
	if err != nil {
		return "", fmt.Errorf("failed to get public IP: %w", err)
	}
	if pipResp.Properties == nil || pipResp.Properties.IPAddress == nil {
		return "", fmt.Errorf("public IP address is not allocated")
	}
	return *pipResp.Properties.IPAddress, nil
}

// CreateSubnet creates a subnet in a VNet.
func (c *AzureClient) CreateSubnet(ctx context.Context, vnetRG, vnetName, subnetName, addressPrefix string) error {
	_, err := c.subnetClient.Get(ctx, vnetRG, vnetName, subnetName, nil)
	if err == nil {
		c.logger.Infof("Subnet %s already exists", subnetName)
		return nil
	}

	poller, err := c.subnetClient.BeginCreateOrUpdate(ctx, vnetRG, vnetName, subnetName, armnetwork.Subnet{
		Properties: &armnetwork.SubnetPropertiesFormat{
			AddressPrefix: ptr(addressPrefix),
		},
	}, nil)
	if err != nil {
		return fmt.Errorf("failed to create subnet: %w", err)
	}
	if _, err = poller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("failed to create subnet: %w", err)
	}
	return nil
}

// CreateNSG creates a network security group with SSH and VPN rules.
func (c *AzureClient) CreateNSG(ctx context.Context, resourceGroup, nsgName, location string, vpnPort int) error {
	_, err := c.nsgClient.Get(ctx, resourceGroup, nsgName, nil)
	if err == nil {
		c.logger.Infof("NSG %s already exists", nsgName)
		return nil
	}

	nsg := armnetwork.SecurityGroup{
		Location: ptr(location),
		Properties: &armnetwork.SecurityGroupPropertiesFormat{
			SecurityRules: []*armnetwork.SecurityRule{
				{
					Name: ptr("allow-ssh"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Priority:                 ptr[int32](100),
						Protocol:                 ptr(armnetwork.SecurityRuleProtocolTCP),
						Access:                   ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefix:      ptr("*"),
						SourcePortRange:          ptr("*"),
						DestinationAddressPrefix: ptr("*"),
						DestinationPortRanges:    []*string{ptr("22")},
					},
				},
				{
					Name: ptr("allow-vpn"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Priority:                 ptr[int32](200),
						Protocol:                 ptr(armnetwork.SecurityRuleProtocolUDP),
						Access:                   ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefix:      ptr("*"),
						SourcePortRange:          ptr("*"),
						DestinationAddressPrefix: ptr("*"),
						DestinationPortRanges:    []*string{ptr(fmt.Sprintf("%d", vpnPort))},
					},
				},
			},
		},
	}

	poller, err := c.nsgClient.BeginCreateOrUpdate(ctx, resourceGroup, nsgName, nsg, nil)
	if err != nil {
		return fmt.Errorf("failed to create NSG: %w", err)
	}
	if _, err = poller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("failed to create NSG: %w", err)
	}
	return nil
}

// CreatePublicIP creates a static public IP address.
func (c *AzureClient) CreatePublicIP(ctx context.Context, resourceGroup, pipName, location string) error {
	_, err := c.pipClient.Get(ctx, resourceGroup, pipName, nil)
	if err == nil {
		c.logger.Infof("Public IP %s already exists", pipName)
		return nil
	}

	poller, err := c.pipClient.BeginCreateOrUpdate(ctx, resourceGroup, pipName, armnetwork.PublicIPAddress{
		Location: ptr(location),
		SKU: &armnetwork.PublicIPAddressSKU{
			Name: ptr(armnetwork.PublicIPAddressSKUNameStandard),
		},
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: ptr(armnetwork.IPAllocationMethodStatic),
		},
	}, nil)
	if err != nil {
		return fmt.Errorf("failed to create public IP: %w", err)
	}
	if _, err = poller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("failed to create public IP: %w", err)
	}
	return nil
}

// GetPublicIPAddress retrieves a public IP address value.
func (c *AzureClient) GetPublicIPAddress(ctx context.Context, resourceGroup, pipName string) (string, error) {
	resp, err := c.pipClient.Get(ctx, resourceGroup, pipName, nil)
	if err != nil {
		return "", fmt.Errorf("failed to get public IP: %w", err)
	}
	if resp.Properties == nil || resp.Properties.IPAddress == nil {
		return "", fmt.Errorf("public IP address is not allocated")
	}
	return *resp.Properties.IPAddress, nil
}

// CreateVM creates a NIC and VM with the specified configuration.
func (c *AzureClient) CreateVM(ctx context.Context, resourceGroup, vmName, location, vnetRG, vnetName, subnetName, nsgName, pipName, sshKeyPath, vmSize string) error {
	pubKeyData, err := ReadFileContent(sshKeyPath + ".pub")
	if err != nil {
		return fmt.Errorf("failed to read SSH public key: %w", err)
	}
	pubKey := strings.TrimSpace(pubKeyData)

	subnetID := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/virtualNetworks/%s/subnets/%s",
		c.subscriptionID, vnetRG, vnetName, subnetName)
	nsgID := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/securityGroups/%s",
		c.subscriptionID, resourceGroup, nsgName)
	pipID := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/publicIPAddresses/%s",
		c.subscriptionID, resourceGroup, pipName)

	nicName := vmName + "VMNic"
	nicPoller, err := c.nicClient.BeginCreateOrUpdate(ctx, resourceGroup, nicName, armnetwork.Interface{
		Location: ptr(location),
		Properties: &armnetwork.InterfacePropertiesFormat{
			NetworkSecurityGroup: &armnetwork.SecurityGroup{
				ID: ptr(nsgID),
			},
			IPConfigurations: []*armnetwork.InterfaceIPConfiguration{
				{
					Name: ptr("ipconfig1"),
					Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
						Subnet: &armnetwork.Subnet{
							ID: ptr(subnetID),
						},
						PublicIPAddress: &armnetwork.PublicIPAddress{
							ID: ptr(pipID),
						},
						PrivateIPAllocationMethod: ptr(armnetwork.IPAllocationMethodDynamic),
					},
				},
			},
		},
	}, nil)
	if err != nil {
		return fmt.Errorf("failed to create NIC: %w", err)
	}
	nicResp, err := nicPoller.PollUntilDone(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to create NIC: %w", err)
	}

	vm := armcompute.VirtualMachine{
		Location: ptr(location),
		Zones:    []*string{ptr("1")},
		Properties: &armcompute.VirtualMachineProperties{
			HardwareProfile: &armcompute.HardwareProfile{
				VMSize: ptr(armcompute.VirtualMachineSizeTypes(vmSize)),
			},
			StorageProfile: &armcompute.StorageProfile{
				ImageReference: &armcompute.ImageReference{
					Publisher: ptr("Canonical"),
					Offer:     ptr("0001-com-ubuntu-server-jammy"),
					SKU:       ptr("22_04-lts-gen2"),
					Version:   ptr("latest"),
				},
				OSDisk: &armcompute.OSDisk{
					CreateOption: ptr(armcompute.DiskCreateOptionTypesFromImage),
					DeleteOption: ptr(armcompute.DiskDeleteOptionTypesDelete),
					ManagedDisk: &armcompute.ManagedDiskParameters{
						StorageAccountType: ptr(armcompute.StorageAccountTypesPremiumLRS),
					},
				},
			},
			OSProfile: &armcompute.OSProfile{
				ComputerName:  ptr(vmName),
				AdminUsername: ptr("azureuser"),
				LinuxConfiguration: &armcompute.LinuxConfiguration{
					DisablePasswordAuthentication: ptr(true),
					SSH: &armcompute.SSHConfiguration{
						PublicKeys: []*armcompute.SSHPublicKey{
							{
								Path:    ptr("/home/azureuser/.ssh/authorized_keys"),
								KeyData: ptr(pubKey),
							},
						},
					},
				},
			},
			NetworkProfile: &armcompute.NetworkProfile{
				NetworkInterfaces: []*armcompute.NetworkInterfaceReference{
					{
						ID: nicResp.ID,
						Properties: &armcompute.NetworkInterfaceReferenceProperties{
							Primary:      ptr(true),
							DeleteOption: ptr(armcompute.DeleteOptionsDelete),
						},
					},
				},
			},
		},
	}

	vmPoller, err := c.vmClient.BeginCreateOrUpdate(ctx, resourceGroup, vmName, vm, nil)
	if err != nil {
		return fmt.Errorf("failed to create VM: %w", err)
	}
	if _, err = vmPoller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("failed to create VM: %w", err)
	}
	return nil
}

// AddSSHKeyToVM adds an SSH key to a VM using RunCommand.
func (c *AzureClient) AddSSHKeyToVM(ctx context.Context, resourceGroup, vmName, sshKeyPath string) error {
	pubKey, err := ReadFileContent(sshKeyPath + ".pub")
	if err != nil {
		return fmt.Errorf("failed to read SSH public key: %w", err)
	}

	script := fmt.Sprintf(
		"mkdir -p /home/azureuser/.ssh && echo '%s' >> /home/azureuser/.ssh/authorized_keys && "+
			"sort -u -o /home/azureuser/.ssh/authorized_keys /home/azureuser/.ssh/authorized_keys && "+
			"chown -R azureuser:azureuser /home/azureuser/.ssh && "+
			"chmod 700 /home/azureuser/.ssh && chmod 600 /home/azureuser/.ssh/authorized_keys",
		strings.TrimSpace(pubKey))

	poller, err := c.vmClient.BeginRunCommand(ctx, resourceGroup, vmName, armcompute.RunCommandInput{
		CommandID: ptr("RunShellScript"),
		Script:    []*string{ptr(script)},
	}, nil)
	if err != nil {
		return fmt.Errorf("failed to run SSH key command: %w", err)
	}
	if _, err = poller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("failed to add SSH key to VM: %w", err)
	}
	return nil
}

// RestartVM restarts a VM.
func (c *AzureClient) RestartVM(ctx context.Context, resourceGroup, vmName string) error {
	poller, err := c.vmClient.BeginRestart(ctx, resourceGroup, vmName, nil)
	if err != nil {
		return fmt.Errorf("failed to restart VM: %w", err)
	}
	if _, err = poller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("failed to restart VM: %w", err)
	}
	return nil
}

// DeleteVM deletes a VM if it exists.
func (c *AzureClient) DeleteVM(ctx context.Context, resourceGroup, vmName string) error {
	if !c.VMExists(ctx, resourceGroup, vmName) {
		return nil
	}
	forceDeletion := true
	poller, err := c.vmClient.BeginDelete(ctx, resourceGroup, vmName, &armcompute.VirtualMachinesClientBeginDeleteOptions{
		ForceDeletion: &forceDeletion,
	})
	if err != nil {
		if isNotFoundError(err) {
			return nil
		}
		return fmt.Errorf("failed to delete VM: %w", err)
	}
	if _, err = poller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("failed to delete VM: %w", err)
	}
	return nil
}

// DeletePublicIP deletes a public IP address if it exists.
func (c *AzureClient) DeletePublicIP(ctx context.Context, resourceGroup, pipName string) error {
	poller, err := c.pipClient.BeginDelete(ctx, resourceGroup, pipName, nil)
	if err != nil {
		if isNotFoundError(err) {
			return nil
		}
		return fmt.Errorf("failed to delete public IP: %w", err)
	}
	if _, err = poller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("failed to delete public IP: %w", err)
	}
	return nil
}

// DeleteNSG deletes a network security group if it exists.
func (c *AzureClient) DeleteNSG(ctx context.Context, resourceGroup, nsgName string) error {
	poller, err := c.nsgClient.BeginDelete(ctx, resourceGroup, nsgName, nil)
	if err != nil {
		if isNotFoundError(err) {
			return nil
		}
		return fmt.Errorf("failed to delete NSG: %w", err)
	}
	if _, err = poller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("failed to delete NSG: %w", err)
	}
	return nil
}

// DeleteSubnet deletes a subnet (errors are ignored).
func (c *AzureClient) DeleteSubnet(ctx context.Context, vnetRG, vnetName, subnetName string) error {
	poller, err := c.subnetClient.BeginDelete(ctx, vnetRG, vnetName, subnetName, nil)
	if err != nil {
		return nil // Ignore errors
	}
	_, _ = poller.PollUntilDone(ctx, nil)
	return nil
}

// GetAKSCredentials gets AKS cluster credentials and writes the kubeconfig to the specified path.
func (c *AzureClient) GetAKSCredentials(ctx context.Context, resourceGroup, clusterName, kubeconfigPath string) error {
	resp, err := c.aksClient.ListClusterUserCredentials(ctx, resourceGroup, clusterName, nil)
	if err != nil {
		return fmt.Errorf("failed to get AKS credentials: %w", err)
	}
	if len(resp.Kubeconfigs) == 0 || resp.Kubeconfigs[0].Value == nil {
		return fmt.Errorf("no kubeconfig returned for cluster %s", clusterName)
	}

	if err := EnsureDirectory(filepath.Dir(kubeconfigPath)); err != nil {
		return fmt.Errorf("failed to create kubeconfig directory: %w", err)
	}
	if err := os.WriteFile(kubeconfigPath, resp.Kubeconfigs[0].Value, 0600); err != nil {
		return fmt.Errorf("failed to write kubeconfig: %w", err)
	}
	return nil
}

// ptr returns a pointer to the given value.
func ptr[T any](v T) *T {
	return &v
}

// isNotFoundError checks if an error is a 404 Not Found response.
func isNotFoundError(err error) bool {
	var respErr *azcore.ResponseError
	return errors.As(err, &respErr) && respErr.StatusCode == http.StatusNotFound
}

// parseResourceGroupAndName extracts resource group and resource name from an Azure resource ID.
func parseResourceGroupAndName(resourceID string) (resourceGroup, name string) {
	parts := strings.Split(resourceID, "/")
	for i, part := range parts {
		if strings.EqualFold(part, "resourceGroups") && i+1 < len(parts) {
			resourceGroup = parts[i+1]
		}
	}
	if len(parts) > 0 {
		name = parts[len(parts)-1]
	}
	return
}
