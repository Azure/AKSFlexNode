package privatecluster

// CleanupMode defines the cleanup mode for uninstallation
type CleanupMode string

const (
	CleanupModeLocal CleanupMode = "local"
	CleanupModeFull  CleanupMode = "full"
)

// GatewayConfig holds configuration for the VPN Gateway VM
type GatewayConfig struct {
	Name         string
	SubnetName   string
	SubnetPrefix string
	VMSize       string
	Port         int
}

// VPNConfig holds VPN connection configuration
type VPNConfig struct {
	NetworkInterface string
	VPNNetwork       string
	GatewayVPNIP     string
	ClientVPNIP      string
	ServerPublicKey  string
	ServerEndpoint   string
}

// AKSClusterInfo holds parsed AKS cluster information
type AKSClusterInfo struct {
	ResourceID        string
	SubscriptionID    string
	ResourceGroup     string
	ClusterName       string
	Location          string
	TenantID          string
	NodeResourceGroup string
	VNetName          string
	VNetResourceGroup string
	PrivateFQDN       string
	APIServerIP       string
}

// SSHConfig holds SSH connection configuration
type SSHConfig struct {
	KeyPath string
	Host    string
	User    string
	Port    int
	Timeout int
}

// DefaultGatewayConfig returns the default Gateway configuration
func DefaultGatewayConfig() GatewayConfig {
	return GatewayConfig{
		Name:         "wg-gateway",
		SubnetName:   "wg-subnet",
		SubnetPrefix: "10.0.100.0/24",
		VMSize:       "Standard_D2s_v3",
		Port:         51820,
	}
}

// DefaultVPNConfig returns the default VPN configuration
func DefaultVPNConfig() VPNConfig {
	return VPNConfig{
		NetworkInterface: "wg-aks",
		VPNNetwork:       "172.16.0.0/24",
		GatewayVPNIP:     "172.16.0.1",
	}
}

// DefaultSSHConfig returns the default SSH configuration
func DefaultSSHConfig(keyPath, host string) SSHConfig {
	return SSHConfig{
		KeyPath: keyPath,
		Host:    host,
		User:    "azureuser",
		Port:    22,
		Timeout: 10,
	}
}
