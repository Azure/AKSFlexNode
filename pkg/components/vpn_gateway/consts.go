package vpn_gateway

import (
	"path/filepath"
	"time"
)

const (
	// VPN Gateway default name
	defaultVPNGatewayName = "vpn-gateway"

	// Azure VPN Gateway configuration
	vpnClientRootCertName = "VPNClientRootCert"
	gatewaySubnetName     = "GatewaySubnet"
	gatewaySubnetPrefix   = 27 // /27 subnet for GatewaySubnet

	// Directory paths
	systemConfigDir  = "/etc/aks-flex-node"
	certificatesDir  = "/etc/aks-flex-node/certs"
	openVPNConfigDir = "/etc/openvpn"

	// File names
	vpnConfigFileName     = "vpn-config.ovpn"
	vpnClientCertFileName = "vpn-client.crt"
	vpnClientKeyFileName  = "vpn-client.key"
	vpnRootCertFileName   = "vpn-root-ca.crt"
	openVPNConfigFileName = "vpnconfig.conf"

	// File permissions
	certificatesDirPerm = 0700
	configDirPerm       = 0755
	privateKeyFilePerm  = 0600
	certificateFilePerm = 0644

	// Certificate configuration
	certificateKeySize    = 2048
	certificateValidYears = 10
	certificateCommonName = "VPN CA"

	// PEM block types
	rsaPrivateKeyType = "RSA PRIVATE KEY"
	certificateType   = "CERTIFICATE"

	// Timeouts and intervals
	gatewayProvisioningTimeout = 30 * time.Minute // VPN Gateway provisioning timeout
	gatewayStatusCheckInterval = 30 * time.Second // Polling interval for gateway status
	vpnConnectionTimeout       = 1 * time.Minute  // VPN connection establishment timeout
	vpnConnectionCheckInterval = 2 * time.Second  // Interval for VPN connection checks

	// System paths for validation
	systemEtcPrefix = "/etc/"
	systemUsrPrefix = "/usr/"
	systemVarPrefix = "/var/"

	// Temporary file patterns
	tempVPNConfigPattern = "vpnconfig-*.ovpn"
	tempVPNCertPattern   = "vpn-cert-*.tmp"
	tempVPNZipPattern    = "vpnconfig-*.zip"
	tempVPNExtractPrefix = "vpnconfig-"

	// OpenVPN service template
	openVPNServiceTemplate = "openvpn@vpnconfig"
	openVPNServiceName     = "vpnconfig"

	// Public IP naming pattern
	gatewayPublicIPName = "vpn-gateway-ip"
	vpnGatewayName      = "vpn-gateway"

	// Point-to-Site configuration name
	p2sConfigName = "P2SConfig"
)

// GetVPNClientCertPath returns the full path to the VPN client certificate file
func GetVPNClientCertPath() string {
	return filepath.Join(certificatesDir, vpnClientCertFileName)
}

// GetVPNClientKeyPath returns the full path to the VPN client private key file
func GetVPNClientKeyPath() string {
	return filepath.Join(certificatesDir, vpnClientKeyFileName)
}

// GetVPNRootCertPath returns the full path to the VPN root CA certificate file
func GetVPNRootCertPath() string {
	return filepath.Join(certificatesDir, vpnRootCertFileName)
}

// GetOpenVPNConfigPath returns the full path to the OpenVPN configuration file
func GetOpenVPNConfigPath() string {
	return filepath.Join(openVPNConfigDir, openVPNConfigFileName)
}

// GetVPNConfigPath returns the full path to the VPN configuration file in system config directory
func GetVPNConfigPath() string {
	return filepath.Join(systemConfigDir, vpnConfigFileName)
}
