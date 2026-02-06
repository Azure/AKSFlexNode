package privatecluster

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// AzureCLI provides Azure CLI operations
type AzureCLI struct {
	logger *Logger
}

// NewAzureCLI creates a new AzureCLI instance
func NewAzureCLI(logger *Logger) *AzureCLI {
	return &AzureCLI{logger: logger}
}

// CheckInstalled verifies Azure CLI is installed
func (az *AzureCLI) CheckInstalled() error {
	if !CommandExists("az") {
		return fmt.Errorf("azure CLI not installed, please install: curl -sL https://aka.ms/InstallAzureCLIDeb | sudo bash")
	}
	return nil
}

// CheckLogin verifies Azure CLI is logged in
func (az *AzureCLI) CheckLogin(ctx context.Context) error {
	if !RunCommandSilent(ctx, "az", "account", "show") {
		return fmt.Errorf("azure CLI not logged in, please run 'az login' first")
	}
	return nil
}

// CheckAndRefreshToken checks if token is valid and refreshes if needed
func (az *AzureCLI) CheckAndRefreshToken(ctx context.Context) error {
	if !RunCommandSilent(ctx, "az", "account", "get-access-token", "--only-show-errors") {
		az.logger.Warning("Azure token expired or invalid, re-authenticating...")
		return RunCommandInteractive(ctx, "az", "login")
	}
	return nil
}

// SetSubscription sets the active subscription
func (az *AzureCLI) SetSubscription(ctx context.Context, subscriptionID string) error {
	_, err := RunCommand(ctx, "az", "account", "set", "--subscription", subscriptionID)
	return err
}

// GetTenantID returns the current tenant ID
func (az *AzureCLI) GetTenantID(ctx context.Context) (string, error) {
	return RunCommand(ctx, "az", "account", "show", "--query", "tenantId", "-o", "tsv")
}

// AKSClusterExists checks if an AKS cluster exists
func (az *AzureCLI) AKSClusterExists(ctx context.Context, resourceGroup, clusterName string) bool {
	return RunCommandSilent(ctx, "az", "aks", "show",
		"--resource-group", resourceGroup,
		"--name", clusterName)
}

// GetAKSClusterInfo retrieves AKS cluster information
func (az *AzureCLI) GetAKSClusterInfo(ctx context.Context, resourceGroup, clusterName string) (*AKSClusterInfo, error) {
	info := &AKSClusterInfo{
		ResourceGroup: resourceGroup,
		ClusterName:   clusterName,
	}

	// Get AAD enabled status
	aadEnabled, _ := RunCommand(ctx, "az", "aks", "show",
		"--resource-group", resourceGroup,
		"--name", clusterName,
		"--query", "aadProfile.managed", "-o", "tsv")

	if strings.ToLower(aadEnabled) != "true" {
		return nil, fmt.Errorf("AKS cluster AAD not enabled, please enable: az aks update --enable-aad")
	}

	// Get RBAC enabled status
	rbacEnabled, _ := RunCommand(ctx, "az", "aks", "show",
		"--resource-group", resourceGroup,
		"--name", clusterName,
		"--query", "aadProfile.enableAzureRbac", "-o", "tsv")

	if strings.ToLower(rbacEnabled) != "true" {
		return nil, fmt.Errorf("AKS cluster Azure RBAC not enabled, please enable: az aks update --enable-azure-rbac")
	}

	// Get location
	location, err := RunCommand(ctx, "az", "aks", "show",
		"--resource-group", resourceGroup,
		"--name", clusterName,
		"--query", "location", "-o", "tsv")
	if err != nil {
		return nil, fmt.Errorf("failed to get cluster location: %w", err)
	}
	info.Location = location

	// Get node resource group
	nodeRG, err := RunCommand(ctx, "az", "aks", "show",
		"--resource-group", resourceGroup,
		"--name", clusterName,
		"--query", "nodeResourceGroup", "-o", "tsv")
	if err != nil {
		return nil, fmt.Errorf("failed to get node resource group: %w", err)
	}
	info.NodeResourceGroup = nodeRG

	// Get private FQDN
	privateFQDN, err := RunCommand(ctx, "az", "aks", "show",
		"--resource-group", resourceGroup,
		"--name", clusterName,
		"--query", "privateFqdn", "-o", "tsv")
	if err != nil {
		return nil, fmt.Errorf("failed to get private FQDN: %w", err)
	}
	info.PrivateFQDN = privateFQDN

	return info, nil
}

// GetVNetInfo retrieves VNet information from AKS VMSS
func (az *AzureCLI) GetVNetInfo(ctx context.Context, nodeResourceGroup string) (vnetName, vnetRG string, err error) {
	// Get first VMSS name
	vmssName, err := RunCommand(ctx, "az", "vmss", "list",
		"--resource-group", nodeResourceGroup,
		"--query", "[0].name", "-o", "tsv")
	if err != nil || vmssName == "" {
		return "", "", fmt.Errorf("cannot find AKS node VMSS in %s", nodeResourceGroup)
	}

	// Get subnet ID from VMSS
	subnetID, err := RunCommand(ctx, "az", "vmss", "show",
		"--resource-group", nodeResourceGroup,
		"--name", vmssName,
		"--query", "virtualMachineProfile.networkProfile.networkInterfaceConfigurations[0].ipConfigurations[0].subnet.id",
		"-o", "tsv")
	if err != nil {
		return "", "", fmt.Errorf("failed to get subnet ID from VMSS: %w", err)
	}

	// Parse VNet name and resource group from subnet ID
	// Format: /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.Network/virtualNetworks/{vnet}/subnets/{subnet}
	parts := strings.Split(subnetID, "/")
	if len(parts) < 9 {
		return "", "", fmt.Errorf("invalid subnet ID format: %s", subnetID)
	}

	vnetRG = parts[4]
	vnetName = parts[8]

	return vnetName, vnetRG, nil
}

// VMExists checks if a VM exists
func (az *AzureCLI) VMExists(ctx context.Context, resourceGroup, vmName string) bool {
	return RunCommandSilent(ctx, "az", "vm", "show",
		"--resource-group", resourceGroup,
		"--name", vmName)
}

// GetVMPublicIP retrieves a VM's public IP address
func (az *AzureCLI) GetVMPublicIP(ctx context.Context, resourceGroup, vmName string) (string, error) {
	return RunCommand(ctx, "az", "vm", "list-ip-addresses",
		"--resource-group", resourceGroup,
		"--name", vmName,
		"--query", "[0].virtualMachine.network.publicIpAddresses[0].ipAddress",
		"-o", "tsv")
}

// CreateSubnet creates a subnet in a VNet
func (az *AzureCLI) CreateSubnet(ctx context.Context, vnetRG, vnetName, subnetName, addressPrefix string) error {
	// Check if subnet exists
	if RunCommandSilent(ctx, "az", "network", "vnet", "subnet", "show",
		"--resource-group", vnetRG,
		"--vnet-name", vnetName,
		"--name", subnetName) {
		az.logger.Info("Subnet %s already exists", subnetName)
		return nil
	}

	_, err := RunCommand(ctx, "az", "network", "vnet", "subnet", "create",
		"--resource-group", vnetRG,
		"--vnet-name", vnetName,
		"--name", subnetName,
		"--address-prefixes", addressPrefix)
	return err
}

// CreateNSG creates a network security group with rules
func (az *AzureCLI) CreateNSG(ctx context.Context, resourceGroup, nsgName string, vpnPort int) error {
	// Check if NSG exists
	if RunCommandSilent(ctx, "az", "network", "nsg", "show",
		"--resource-group", resourceGroup,
		"--name", nsgName) {
		az.logger.Info("NSG %s already exists", nsgName)
		return nil
	}

	// Create NSG
	if _, err := RunCommand(ctx, "az", "network", "nsg", "create",
		"--resource-group", resourceGroup,
		"--name", nsgName); err != nil {
		return fmt.Errorf("failed to create NSG: %w", err)
	}

	// Add SSH rule (priority 100 to override NRMS-Rule-106)
	if _, err := RunCommand(ctx, "az", "network", "nsg", "rule", "create",
		"--resource-group", resourceGroup,
		"--nsg-name", nsgName,
		"--name", "allow-ssh",
		"--priority", "100",
		"--destination-port-ranges", "22",
		"--protocol", "Tcp",
		"--access", "Allow"); err != nil {
		return fmt.Errorf("failed to create SSH rule: %w", err)
	}

	// Add VPN rule
	if _, err := RunCommand(ctx, "az", "network", "nsg", "rule", "create",
		"--resource-group", resourceGroup,
		"--nsg-name", nsgName,
		"--name", "allow-vpn",
		"--priority", "200",
		"--destination-port-ranges", fmt.Sprintf("%d", vpnPort),
		"--protocol", "Udp",
		"--access", "Allow"); err != nil {
		return fmt.Errorf("failed to create VPN rule: %w", err)
	}

	return nil
}

// CreatePublicIP creates a static public IP
func (az *AzureCLI) CreatePublicIP(ctx context.Context, resourceGroup, pipName string) error {
	// Check if PIP exists
	if RunCommandSilent(ctx, "az", "network", "public-ip", "show",
		"--resource-group", resourceGroup,
		"--name", pipName) {
		az.logger.Info("Public IP %s already exists", pipName)
		return nil
	}

	_, err := RunCommand(ctx, "az", "network", "public-ip", "create",
		"--resource-group", resourceGroup,
		"--name", pipName,
		"--sku", "Standard",
		"--allocation-method", "Static")
	return err
}

// GetPublicIPAddress retrieves a public IP address
func (az *AzureCLI) GetPublicIPAddress(ctx context.Context, resourceGroup, pipName string) (string, error) {
	return RunCommand(ctx, "az", "network", "public-ip", "show",
		"--resource-group", resourceGroup,
		"--name", pipName,
		"--query", "ipAddress", "-o", "tsv")
}

// CreateVM creates a VM with specified configuration
func (az *AzureCLI) CreateVM(ctx context.Context, resourceGroup, vmName, vnetName, subnetName, nsgName, pipName, sshKeyPath, vmSize string) error {
	_, err := RunCommand(ctx, "az", "vm", "create",
		"--resource-group", resourceGroup,
		"--name", vmName,
		"--image", "Ubuntu2204",
		"--size", vmSize,
		"--vnet-name", vnetName,
		"--subnet", subnetName,
		"--nsg", nsgName,
		"--public-ip-address", pipName,
		"--admin-username", "azureuser",
		"--ssh-key-values", sshKeyPath+".pub",
		"--zone", "1")
	return err
}

// AddSSHKeyToVM adds an SSH key to a VM
func (az *AzureCLI) AddSSHKeyToVM(ctx context.Context, resourceGroup, vmName, sshKeyPath string) error {
	pubKey, err := ReadFileContent(sshKeyPath + ".pub")
	if err != nil {
		return fmt.Errorf("failed to read SSH public key: %w", err)
	}

	_, err = RunCommand(ctx, "az", "vm", "user", "update",
		"--resource-group", resourceGroup,
		"--name", vmName,
		"--username", "azureuser",
		"--ssh-key-value", strings.TrimSpace(pubKey),
		"--output", "none")
	return err
}

// RestartVM restarts a VM
func (az *AzureCLI) RestartVM(ctx context.Context, resourceGroup, vmName string) error {
	_, err := RunCommand(ctx, "az", "vm", "restart",
		"--resource-group", resourceGroup,
		"--name", vmName,
		"--no-wait")
	return err
}

// DeleteVM deletes a VM
func (az *AzureCLI) DeleteVM(ctx context.Context, resourceGroup, vmName string) error {
	if !az.VMExists(ctx, resourceGroup, vmName) {
		return nil
	}
	_, err := RunCommand(ctx, "az", "vm", "delete",
		"--resource-group", resourceGroup,
		"--name", vmName,
		"--yes",
		"--only-show-errors")
	return err
}

// DeleteNIC deletes a network interface
func (az *AzureCLI) DeleteNIC(ctx context.Context, resourceGroup, nicName string) error {
	if !RunCommandSilent(ctx, "az", "network", "nic", "show",
		"--resource-group", resourceGroup,
		"--name", nicName) {
		return nil
	}
	_, err := RunCommand(ctx, "az", "network", "nic", "delete",
		"--resource-group", resourceGroup,
		"--name", nicName,
		"--only-show-errors")
	return err
}

// DeletePublicIP deletes a public IP
func (az *AzureCLI) DeletePublicIP(ctx context.Context, resourceGroup, pipName string) error {
	if !RunCommandSilent(ctx, "az", "network", "public-ip", "show",
		"--resource-group", resourceGroup,
		"--name", pipName) {
		return nil
	}
	_, err := RunCommand(ctx, "az", "network", "public-ip", "delete",
		"--resource-group", resourceGroup,
		"--name", pipName,
		"--only-show-errors")
	return err
}

// DeleteNSG deletes a network security group
func (az *AzureCLI) DeleteNSG(ctx context.Context, resourceGroup, nsgName string) error {
	if !RunCommandSilent(ctx, "az", "network", "nsg", "show",
		"--resource-group", resourceGroup,
		"--name", nsgName) {
		return nil
	}
	_, err := RunCommand(ctx, "az", "network", "nsg", "delete",
		"--resource-group", resourceGroup,
		"--name", nsgName,
		"--only-show-errors")
	return err
}

// DeleteSubnet deletes a subnet
func (az *AzureCLI) DeleteSubnet(ctx context.Context, vnetRG, vnetName, subnetName string) error {
	_, _ = RunCommand(ctx, "az", "network", "vnet", "subnet", "delete",
		"--resource-group", vnetRG,
		"--vnet-name", vnetName,
		"--name", subnetName)
	return nil // Ignore errors
}

// DeleteDisks deletes disks matching a pattern
func (az *AzureCLI) DeleteDisks(ctx context.Context, resourceGroup, pattern string) error {
	output, err := RunCommand(ctx, "az", "disk", "list",
		"--resource-group", resourceGroup,
		"--query", fmt.Sprintf("[?contains(name, '%s')].name", pattern),
		"-o", "json")
	if err != nil {
		return nil // Ignore errors
	}

	var diskNames []string
	if err := json.Unmarshal([]byte(output), &diskNames); err != nil {
		return nil
	}

	for _, disk := range diskNames {
		_, _ = RunCommand(ctx, "az", "disk", "delete",
			"--resource-group", resourceGroup,
			"--name", disk,
			"--yes",
			"--only-show-errors")
	}

	return nil
}

// DeleteConnectedMachine deletes an Arc connected machine
func (az *AzureCLI) DeleteConnectedMachine(ctx context.Context, resourceGroup, machineName string) error {
	_, _ = RunCommand(ctx, "az", "connectedmachine", "delete",
		"--resource-group", resourceGroup,
		"--name", machineName,
		"--yes")
	return nil // Ignore errors
}

// GetAKSCredentials gets AKS cluster credentials
func (az *AzureCLI) GetAKSCredentials(ctx context.Context, resourceGroup, clusterName, kubeconfigPath string) error {
	// Ensure directory exists
	if err := EnsureDirectory("/root/.kube"); err != nil {
		return err
	}

	_, err := RunCommand(ctx, "az", "aks", "get-credentials",
		"--resource-group", resourceGroup,
		"--name", clusterName,
		"--overwrite-existing",
		"--file", kubeconfigPath)
	return err
}

// InstallAKSCLI installs kubectl and kubelogin
func (az *AzureCLI) InstallAKSCLI(ctx context.Context) error {
	_, err := RunCommand(ctx, "az", "aks", "install-cli",
		"--install-location", "/usr/local/bin/kubectl",
		"--kubelogin-install-location", "/usr/local/bin/kubelogin")
	if err != nil {
		return err
	}

	_, _ = RunCommand(ctx, "chmod", "+x", "/usr/local/bin/kubectl", "/usr/local/bin/kubelogin")
	return nil
}

// InstallConnectedMachineExtension installs the connectedmachine extension
func (az *AzureCLI) InstallConnectedMachineExtension(ctx context.Context) error {
	// Check if already installed
	if RunCommandSilent(ctx, "az", "extension", "show", "--name", "connectedmachine") {
		return nil
	}

	_, _ = RunCommand(ctx, "az", "config", "set", "extension.dynamic_install_allow_preview=true", "--only-show-errors")

	// Install extension
	_, err := RunCommand(ctx, "az", "extension", "add",
		"--name", "connectedmachine",
		"--allow-preview", "true",
		"--only-show-errors")
	return err
}
