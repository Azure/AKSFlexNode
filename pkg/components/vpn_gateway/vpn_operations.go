package vpn_gateway

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v4"
	"github.com/apparentlymart/go-cidr/cidr"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
)

// VPN certificate and connection management functions for the Installer

// generateCertificates generates VPN certificates for P2S connection
func (i *Installer) generateCertificates() (string, error) {
	_, err := i.setupCertificateDirectory()
	if err != nil {
		return "", err
	}

	// Check if certificates already exist
	if certBase64, exists := i.loadExistingCertificate(); exists {
		return certBase64, nil
	}

	i.logger.Info("Generating new VPN certificates...")
	certBase64, err := i.generateNewCertificate()
	if err != nil {
		return "", err
	}
	return certBase64, nil // new certificate generated
}

// setupCertificateDirectory creates and configures the certificate directory
func (i *Installer) setupCertificateDirectory() (string, error) {
	certDir := certificatesDir
	if err := utils.RunSystemCommand("mkdir", "-p", certDir); err != nil {
		return "", fmt.Errorf("failed to create certificates directory: %w", err)
	}

	// Set proper permissions on the directory
	if err := utils.RunSystemCommand("chmod", fmt.Sprintf("%o", certificatesDirPerm), certDir); err != nil {
		return "", fmt.Errorf("failed to set permissions on certificates directory: %w", err)
	}

	i.logger.Infof("Using system certificate directory: %s", certDir)

	return certDir, nil
}

// loadExistingCertificate checks for existing certificates and returns root certificate base64 data if found
func (i *Installer) loadExistingCertificate() (string, bool) {
	certPath := GetVPNClientCertPath()
	keyPath := GetVPNClientKeyPath()
	rootCertPath := GetVPNRootCertPath()

	// Check if all required files exist
	if _, err := os.Stat(certPath); err == nil {
		if _, err := os.Stat(keyPath); err == nil {
			if _, err := os.Stat(rootCertPath); err == nil {
				i.logger.Info("VPN certificates already exist, using existing certificates")

				// Read existing root certificate and return base64 data for Azure comparison
				rootCertData, err := os.ReadFile(rootCertPath)
				if err != nil {
					i.logger.Warnf("Failed to read existing root certificate: %v", err)
					return "", false
				}

				// Parse certificate to get DER data for base64 encoding
				block, _ := pem.Decode(rootCertData)
				if block == nil {
					i.logger.Warnf("Failed to parse existing root certificate PEM")
					return "", false
				}

				rootCertBase64 := base64.StdEncoding.EncodeToString(block.Bytes)
				i.logger.Info("Using existing VPN certificates")
				return rootCertBase64, true
			}
		}
	}

	return "", false
}

// generateNewCertificate creates a proper CA hierarchy with root CA and client certificate
func (i *Installer) generateNewCertificate() (string, error) {
	// Generate root CA private key
	rootPrivateKey, err := rsa.GenerateKey(rand.Reader, certificateKeySize)
	if err != nil {
		return "", fmt.Errorf("failed to generate root CA private key: %w", err)
	}

	// Create root CA certificate
	rootCertDER, err := i.createRootCACertificate(rootPrivateKey)
	if err != nil {
		return "", fmt.Errorf("failed to create root CA certificate: %w", err)
	}

	// Generate client private key
	clientPrivateKey, err := rsa.GenerateKey(rand.Reader, certificateKeySize)
	if err != nil {
		return "", fmt.Errorf("failed to generate client private key: %w", err)
	}

	// Create client certificate signed by root CA
	clientCertDER, err := i.createClientCertificate(clientPrivateKey, rootPrivateKey, rootCertDER)
	if err != nil {
		return "", fmt.Errorf("failed to create client certificate: %w", err)
	}

	// Save certificate and key to files
	clientKeyPath := GetVPNClientKeyPath()
	clientCertPath := GetVPNClientCertPath()
	rootCertPath := GetVPNRootCertPath()

	// Save the client private key
	if err := i.savePrivateKey(clientKeyPath, clientPrivateKey); err != nil {
		return "", err
	}

	// Save the client certificate
	if err := i.saveCertificate(clientCertPath, clientCertDER); err != nil {
		return "", err
	}

	// Save the root CA certificate (for Azure upload)
	if err := i.saveCertificate(rootCertPath, rootCertDER); err != nil {
		return "", err
	}

	// Return base64-encoded root certificate for upload to Azure
	rootCertBase64 := base64.StdEncoding.EncodeToString(rootCertDER)

	i.logger.Info("VPN certificate hierarchy generated successfully (root CA + client cert)")

	return rootCertBase64, nil
}

// createRootCACertificate generates a root CA certificate
func (i *Installer) createRootCACertificate(privateKey *rsa.PrivateKey) ([]byte, error) {
	// Generate SubjectKeyIdentifier for the root CA
	publicKeyBytes, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal public key: %w", err)
	}
	subjectKeyID := sha256.Sum256(publicKeyBytes)

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: certificateCommonName,
		},
		NotBefore:             time.Now().Add(-10 * time.Minute),
		NotAfter:              time.Now().Add(certificateValidYears * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,            // This is a CA certificate
		SubjectKeyId:          subjectKeyID[:], // Required for chain validation
	}

	// Self-signed root CA: template is both the certificate to create and the issuer
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create root CA certificate: %w", err)
	}

	return certDER, nil
}

// createClientCertificate generates a client certificate signed by the root CA
func (i *Installer) createClientCertificate(clientPrivateKey, rootPrivateKey *rsa.PrivateKey, rootCertDER []byte) ([]byte, error) {
	// Parse the root certificate to use as issuer
	rootCert, err := x509.ParseCertificate(rootCertDER)
	if err != nil {
		return nil, fmt.Errorf("failed to parse root certificate: %w", err)
	}

	// Ensure the root certificate has SubjectKeyId (should be set from createRootCACertificate)
	if len(rootCert.SubjectKeyId) == 0 {
		return nil, fmt.Errorf("root certificate is missing SubjectKeyId")
	}

	// Generate SubjectKeyIdentifier for the client certificate
	clientPublicKeyBytes, err := x509.MarshalPKIXPublicKey(&clientPrivateKey.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal client public key: %w", err)
	}
	clientSubjectKeyID := sha256.Sum256(clientPublicKeyBytes)

	template := x509.Certificate{
		SerialNumber: big.NewInt(2), // Different serial number for client cert
		Subject: pkix.Name{
			CommonName: "VPN Client",
		},
		NotBefore:             time.Now().Add(-10 * time.Minute),
		NotAfter:              time.Now().Add(certificateValidYears * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,                 // This is NOT a CA certificate
		SubjectKeyId:          clientSubjectKeyID[:], // Required for chain validation
		AuthorityKeyId:        rootCert.SubjectKeyId, // MUST match root's SubjectKeyId exactly
	}

	i.logger.Infof("Creating client certificate with AuthorityKeyId matching root SubjectKeyId: %x", rootCert.SubjectKeyId)

	// Client certificate signed by root CA
	certDER, err := x509.CreateCertificate(rand.Reader, &template, rootCert, &clientPrivateKey.PublicKey, rootPrivateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create client certificate: %w", err)
	}

	return certDER, nil
}

// savePrivateKey saves the private key to file with proper permissions
func (i *Installer) savePrivateKey(keyPath string, privateKey *rsa.PrivateKey) error {
	privateKeyPEM := &pem.Block{
		Type:  rsaPrivateKeyType,
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	}

	// Encode PEM to bytes
	var keyBuffer bytes.Buffer
	if err := pem.Encode(&keyBuffer, privateKeyPEM); err != nil {
		return fmt.Errorf("failed to encode private key: %w", err)
	}

	// Write file using system-level file operations
	if err := utils.WriteFileAtomicSystem(keyPath, keyBuffer.Bytes(), privateKeyFilePerm); err != nil {
		return fmt.Errorf("failed to create key file: %w", err)
	}

	return nil
}

// saveCertificate saves the certificate to file with proper permissions
func (i *Installer) saveCertificate(certPath string, certDER []byte) error {
	certPEM := &pem.Block{
		Type:  certificateType,
		Bytes: certDER,
	}

	// Encode PEM to bytes
	var certBuffer bytes.Buffer
	if err := pem.Encode(&certBuffer, certPEM); err != nil {
		return fmt.Errorf("failed to encode certificate: %w", err)
	}

	// Write file using system-level file operations
	if err := utils.WriteFileAtomicSystem(certPath, certBuffer.Bytes(), certificateFilePerm); err != nil {
		return fmt.Errorf("failed to create certificate file: %w", err)
	}

	return nil
}

// processVPNConfig processes VPN config file and embeds certificates using sed (matching working implementation)
func (i *Installer) processVPNConfig(sourcePath, destPath string) error {
	i.logger.Info("Processing VPN configuration with certificate data...")

	// Ensure certificates exist - use the same directory logic as GenerateCertificates
	_, err := i.setupCertificateDirectory()
	if err != nil {
		return fmt.Errorf("failed to setup certificate directory: %w", err)
	}

	clientCertPath := GetVPNClientCertPath()
	clientKeyPath := GetVPNClientKeyPath()

	// Check if certificates exist, if not generate them
	if _, err := os.Stat(clientCertPath); os.IsNotExist(err) {
		i.logger.Info("Client certificates not found, generating new certificates...")
		if _, err := i.generateCertificates(); err != nil {
			return fmt.Errorf("failed to generate certificates: %w", err)
		}
	}

	// Create a temporary working copy of the VPN config
	tempConfig, err := os.CreateTemp("", tempVPNConfigPattern)
	if err != nil {
		return fmt.Errorf("failed to create temporary config file: %w", err)
	}
	tempConfigPath := tempConfig.Name()
	_ = tempConfig.Close()
	defer func() {
		if err := utils.RunCleanupCommand(tempConfigPath); err != nil {
			i.logger.Warnf("Failed to clean up temp config file %s: %v", tempConfigPath, err)
		}
	}()

	// Copy source to temp file
	if err := utils.RunSystemCommand("cp", sourcePath, tempConfigPath); err != nil {
		return fmt.Errorf("failed to copy VPN config to temp file: %w", err)
	}

	// Read certificate and key content using system commands
	certContent, err := utils.RunCommandWithOutput("cat", clientCertPath)
	if err != nil {
		return fmt.Errorf("failed to read certificate file: %w", err)
	}

	keyContent, err := utils.RunCommandWithOutput("cat", clientKeyPath)
	if err != nil {
		return fmt.Errorf("failed to read private key file: %w", err)
	}

	// Read the config file content
	configContent, err := utils.RunCommandWithOutput("cat", tempConfigPath)
	if err != nil {
		return fmt.Errorf("failed to read temp config file: %w", err)
	}

	// Replace placeholders with actual content (trim to remove any trailing whitespace)
	processedConfig := strings.ReplaceAll(configContent, "$CLIENTCERTIFICATE", strings.TrimSpace(certContent))
	processedConfig = strings.ReplaceAll(processedConfig, "$PRIVATEKEY", strings.TrimSpace(keyContent))

	// Write processed config back to temp file using system command
	if err := utils.WriteFileAtomicSystem(tempConfigPath, []byte(processedConfig), 0600); err != nil {
		return fmt.Errorf("failed to write processed config: %w", err)
	}

	// Copy processed config to final destination
	if strings.HasPrefix(destPath, systemEtcPrefix) || strings.HasPrefix(destPath, systemUsrPrefix) || strings.HasPrefix(destPath, systemVarPrefix) {
		// Create destination directory if it doesn't exist
		destDir := filepath.Dir(destPath)
		if err := utils.RunSystemCommand("mkdir", "-p", destDir); err != nil {
			return fmt.Errorf("failed to create destination directory: %w", err)
		}

		// Copy temp file to destination with sudo
		if err := utils.RunSystemCommand("cp", tempConfigPath, destPath); err != nil {
			return fmt.Errorf("failed to copy VPN config to %s: %w", destPath, err)
		}

		// Set proper permissions and ownership with sudo
		if err := utils.RunSystemCommand("chmod", "600", destPath); err != nil {
			i.logger.Warnf("Failed to set permissions on VPN config: %v", err)
		}

		if err := utils.RunSystemCommand("chown", "root:root", destPath); err != nil {
			i.logger.Warnf("Failed to set ownership on VPN config: %v", err)
		}
	} else {
		// Destination is in user directory, copy directly
		if err := utils.RunSystemCommand("cp", tempConfigPath, destPath); err != nil {
			return fmt.Errorf("failed to copy VPN config to %s: %w", destPath, err)
		}
	}

	i.logger.Info("VPN configuration processed successfully")
	return nil
}

// setupOpenVPN installs and configures OpenVPN
func (i *Installer) setupOpenVPN(configPath string) error {
	i.logger.Info("Setting up OpenVPN...")

	// Check if VPN connection is already established
	if i.isVPNConnected() {
		i.logger.Info("VPN connection is already established, skipping OpenVPN setup")
		return nil
	}

	// Install OpenVPN
	// Check if OpenVPN is already installed
	if err := utils.RunSystemCommand("which", "openvpn"); err == nil {
		i.logger.Info("OpenVPN is already installed, skipping installation")
	} else {
		i.logger.Info("Installing OpenVPN...")
		if err := utils.RunSystemCommand("apt", "install", "-y", "openvpn"); err != nil {
			return fmt.Errorf("failed to install OpenVPN: %w", err)
		}
	}

	// Always ensure certificates are embedded in the OpenVPN config
	destPath := GetOpenVPNConfigPath()

	// If configPath is provided, process it; otherwise process existing config
	sourceConfigPath := configPath
	if sourceConfigPath == "" {
		sourceConfigPath = destPath // Process existing config in place
	}

	// Copy and process VPN config file
	if sourceConfigPath != "" {
		vpnConfigDir := openVPNConfigDir
		if err := utils.RunSystemCommand("mkdir", "-p", vpnConfigDir); err != nil {
			return fmt.Errorf("failed to create OpenVPN config directory: %w", err)
		}

		// Process VPN config with certificate data
		if err := i.processVPNConfig(sourceConfigPath, destPath); err != nil {
			return fmt.Errorf("failed to process VPN config: %w", err)
		}

		// Enable and restart OpenVPN service to ensure it uses the updated configuration
		if err := utils.EnableService(openVPNServiceTemplate); err != nil {
			return fmt.Errorf("failed to enable OpenVPN service: %w", err)
		}

		i.logger.Info("Restarting OpenVPN service to apply updated configuration...")
		if err := utils.RestartService(openVPNServiceTemplate); err != nil {
			return fmt.Errorf("failed to restart OpenVPN service: %w", err)
		}

		// Give OpenVPN a moment to start before checking status
		time.Sleep(2 * time.Second)

		// Check if OpenVPN service started successfully
		if !utils.IsServiceActive(openVPNServiceTemplate) {
			i.logger.Warn("OpenVPN service is not active, please check the service status for details")
		} else {
			i.logger.Info("OpenVPN service restarted successfully")
		}
	}
	return nil
}

// configureVPNNetworking configures routes and iptables rules for VPN gateway connectivity
func (i *Installer) configureVPNNetworking(ctx context.Context, vnetInfo vnetResourceInfo) error {
	i.logger.Info("Configuring VPN network routes and iptables rules...")

	// Get Pod CIDR from user configuration and extract all AKS VNet CIDRs from vnetInfo
	podCIDR, aksVNetCIDRs, err := i.getNetworkConfiguration(vnetInfo)
	if err != nil {
		return fmt.Errorf("failed to get network configuration: %w", err)
	}

	i.logger.Infof("Using AKS VNet CIDRs: %v, Pod CIDR: %s", aksVNetCIDRs, podCIDR)

	// Get VPN interface
	vpnInterface, err := utils.GetVPNInterface()
	if err != nil {
		return fmt.Errorf("failed to get VPN interface: %w", err)
	}

	i.logger.Infof("Configuring networking for VPN interface: %s", vpnInterface)

	// Add route for AKS VNet via VPN gateway
	// The gateway IP is typically the first IP in the P2S CIDR range + 1
	gatewayIP, err := i.calculateGatewayIP()
	if err != nil {
		return fmt.Errorf("failed to calculate gateway IP: %w", err)
	}

	// Add routes for all VNet CIDRs
	for _, vnetCIDR := range aksVNetCIDRs {
		i.logger.Infof("Adding route: %s via %s dev %s", vnetCIDR, gatewayIP, vpnInterface)
		if err := i.addIPRoute(vnetCIDR, gatewayIP, vpnInterface); err != nil {
			return fmt.Errorf("failed to add route for AKS VNet CIDR %s: %w", vnetCIDR, err)
		}
	}

	// Add route for AKS pod network (required for flex pod to aks pod communication)
	// This enables Flex node pods to reach AKS cluster pods (like DNS services)
	i.logger.Infof("Adding route for AKS pod network: %s via %s dev %s", podCIDR, gatewayIP, vpnInterface)
	if err := i.addIPRoute(podCIDR, gatewayIP, vpnInterface); err != nil {
		return fmt.Errorf("failed to add route for AKS pod CIDR %s: %w", podCIDR, err)
	}

	i.logger.Info("VPN network configuration completed successfully")
	return nil
}

// getNetworkConfiguration gets Pod CIDR from user config and extracts AKS VNet CIDRs from vnetInfo
func (i *Installer) getNetworkConfiguration(vnetInfo vnetResourceInfo) (string, []string, error) {
	// Get Pod CIDR from user configuration (required)
	if i.config.Azure.VPNGateway == nil || i.config.Azure.VPNGateway.PodCIDR == "" {
		return "", nil, fmt.Errorf("pod CIDR is required in VPN configuration when enabled, please set it")
	}
	podCIDR := i.config.GetVPNGatewayPodCIDR()

	// Extract all AKS VNet CIDRs from the already discovered VNet info
	// Using all VNet CIDRs ensures we can reach all subnets including AKS nodes
	aksVNetCIDRs, err := i.getVNetCIDRsFromInfo(vnetInfo)
	if err != nil {
		return "", nil, fmt.Errorf("failed to get AKS VNet CIDRs: %w", err)
	}

	return podCIDR, aksVNetCIDRs, nil
}

// getVNetCIDRsFromInfo extracts all VNet CIDRs from vnetInfo
func (i *Installer) getVNetCIDRsFromInfo(vnetInfo vnetResourceInfo) ([]string, error) {
	// Extract all VNet CIDRs from the address space
	if vnetInfo.vnet == nil ||
		vnetInfo.vnet.Properties == nil ||
		vnetInfo.vnet.Properties.AddressSpace == nil ||
		len(vnetInfo.vnet.Properties.AddressSpace.AddressPrefixes) == 0 {
		return nil, fmt.Errorf("VNet has no address prefixes")
	}

	// Extract all address prefixes as VNet CIDRs
	var vnetCIDRs []string
	for _, prefix := range vnetInfo.vnet.Properties.AddressSpace.AddressPrefixes {
		if prefix != nil {
			vnetCIDRs = append(vnetCIDRs, *prefix)
		}
	}

	if len(vnetCIDRs) == 0 {
		return nil, fmt.Errorf("VNet has no valid address prefixes")
	}

	i.logger.Infof("Using VNet CIDRs: %v", vnetCIDRs)
	return vnetCIDRs, nil
}

// calculateGatewayIP calculates the gateway IP from P2S CIDR
func (i *Installer) calculateGatewayIP() (string, error) {
	p2sCIDR := i.config.Azure.VPNGateway.P2SGatewayCIDR
	if p2sCIDR == "" {
		return "", fmt.Errorf("P2S Gateway CIDR not configured")
	}

	// Parse the P2S CIDR (e.g., "192.168.100.0/24")
	_, network, err := net.ParseCIDR(p2sCIDR)
	if err != nil {
		return "", fmt.Errorf("failed to parse P2S CIDR %s: %w", p2sCIDR, err)
	}

	// Gateway IP is typically the first usable IP in the range
	// Use go-cidr to safely get the first host IP (network + 1)
	firstIP, _ := cidr.AddressRange(network)
	gatewayIP := cidr.Inc(firstIP) // Increment by 1 to get the gateway IP
	return gatewayIP.String(), nil
}

// addIPRoute adds an IP route if it doesn't already exist
func (i *Installer) addIPRoute(vnetCIDR, gatewayIP, vpnInterface string) error {
	// Try to add the route, capture combined output to check for "File exists" error
	output, err := utils.RunCommandWithOutput("ip", "route", "add", vnetCIDR, "via", gatewayIP, "dev", vpnInterface)
	if err != nil {
		// Check if route already exists by looking for "File exists" in the output or error
		if strings.Contains(output, "File exists") || strings.Contains(err.Error(), "File exists") {
			i.logger.Infof("Route to %s already exists, skipping", vnetCIDR)
			return nil
		}
		return fmt.Errorf("failed to add route for AKS VNet CIDR %s: %s (exit code: %v)", vnetCIDR, strings.TrimSpace(output), err)
	}

	i.logger.Infof("Added route: %s via %s dev %s", vnetCIDR, gatewayIP, vpnInterface)
	return nil
}

// isVPNConnected checks if VPN connection is active
func (i *Installer) isVPNConnected() bool {
	iface, err := utils.GetVPNInterface()
	if err != nil {
		return false
	}

	ip, err := utils.GetVPNInterfaceIP(iface)
	return err == nil && ip != ""
}

// uploadCertificateToAzure uploads the root certificate to Azure VPN Gateway using Azure SDK
func (i *Installer) uploadCertificateToAzure(ctx context.Context, certData string, vnetInfo vnetResourceInfo) error {
	// Get the current VPN Gateway to update its configuration
	gateway, err := i.vgwClient.Get(ctx, vnetInfo.resourceGroupName, defaultVPNGatewayName, nil)
	if err != nil {
		return fmt.Errorf("failed to get VPN Gateway: %w", err)
	}

	// Check if VPN client configuration exists
	// Look for our specific certificate by name and data
	var existingCertFound bool
	var existingCertMatches bool

	// Check if VPN client configuration exists and has certificates
	if gateway.Properties.VPNClientConfiguration != nil &&
		gateway.Properties.VPNClientConfiguration.VPNClientRootCertificates != nil {
		for _, cert := range gateway.Properties.VPNClientConfiguration.VPNClientRootCertificates {
			if cert.Properties != nil && cert.Properties.PublicCertData != nil {
				// Compare certificate data directly
				if *cert.Properties.PublicCertData == certData {
					i.logger.Infof("VPN certificate already exists on Azure VPN Gateway with name '%s', skipping upload", *cert.Name)
					return nil // Certificate already exists and matches, no need to upload
				}
			}
			// Track if any certificate exists (regardless of name)
			if cert.Name != nil {
				existingCertFound = true
			}
		}
	}

	if existingCertFound && !existingCertMatches {
		i.logger.Info("Adding new VPN root certificate alongside existing certificates")
	} else if !existingCertFound {
		i.logger.Info("No existing VPN root certificates found, uploading first certificate")
	}

	// Ensure the VPN client configuration section exists with required address pool
	if gateway.Properties.VPNClientConfiguration == nil {
		p2sGatewayCIDR := i.config.Azure.VPNGateway.P2SGatewayCIDR
		vpnClientProtocol := armnetwork.VPNClientProtocolOpenVPN

		gateway.Properties.VPNClientConfiguration = &armnetwork.VPNClientConfiguration{
			VPNClientAddressPool: &armnetwork.AddressSpace{
				AddressPrefixes: []*string{&p2sGatewayCIDR},
			},
			VPNClientProtocols: []*armnetwork.VPNClientProtocol{&vpnClientProtocol},
		}
	}

	// Create root certificate parameters with unique name based on certificate data
	certHash := fmt.Sprintf("%x", sha256.Sum256([]byte(certData)))[:8] // Use first 8 chars of hash
	certName := fmt.Sprintf("%s-%s", vpnClientRootCertName, certHash)

	i.logger.Infof("Adding VPN root certificate with name: %s", certName)

	rootCert := armnetwork.VPNClientRootCertificate{
		Name: &certName,
		Properties: &armnetwork.VPNClientRootCertificatePropertiesFormat{
			PublicCertData: &certData,
		},
	}

	// Append the new certificate to existing certificates instead of replacing them
	if gateway.Properties.VPNClientConfiguration.VPNClientRootCertificates == nil {
		gateway.Properties.VPNClientConfiguration.VPNClientRootCertificates = []*armnetwork.VPNClientRootCertificate{}
	}

	// Always append the new certificate (with unique name, no conflicts)
	gateway.Properties.VPNClientConfiguration.VPNClientRootCertificates =
		append(gateway.Properties.VPNClientConfiguration.VPNClientRootCertificates, &rootCert)

	// Update the VPN Gateway with the new certificate configuration
	poller, err := i.vgwClient.BeginCreateOrUpdate(ctx, vnetInfo.resourceGroupName, defaultVPNGatewayName, gateway.VirtualNetworkGateway, nil)
	if err != nil {
		return fmt.Errorf("failed to start VPN Gateway update: %w", err)
	}

	// Wait for the operation to complete
	_, err = poller.PollUntilDone(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to update VPN Gateway with certificate: %w", err)
	}

	if existingCertFound && !existingCertMatches {
		i.logger.Info("VPN certificate added successfully - now have multiple certificates available")
	} else {
		i.logger.Info("VPN certificate uploaded to Azure successfully")
	}
	return nil

}

// downloadVPNClientConfig downloads the VPN client configuration from Azure using Azure SDK
func (i *Installer) downloadVPNClientConfig(ctx context.Context, gatewayName, resourceGroup string) (string, error) {
	i.logger.Info("Downloading VPN client configuration from Azure VPN Gateway...")

	// Generate VPN client configuration
	authMethod := armnetwork.AuthenticationMethodEAPTLS
	req := armnetwork.VPNClientParameters{
		AuthenticationMethod: &authMethod,
	}

	poller, err := i.vgwClient.BeginGenerateVPNProfile(ctx, resourceGroup, gatewayName, req, nil)
	if err != nil {
		return "", fmt.Errorf("failed to start VPN client config generation: %w", err)
	}

	// Wait for the operation to complete
	result, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("failed to generate VPN client config: %w", err)
	}

	if result.Value == nil || *result.Value == "" {
		return "", fmt.Errorf("no VPN client configuration URL returned from Azure")
	}

	downloadURL := *result.Value
	i.logger.Infof("VPN client configuration URL: %s", downloadURL)

	// Download the configuration file
	configData, err := i.downloadConfigFromURL(downloadURL)
	if err != nil {
		return "", fmt.Errorf("failed to download VPN client configuration: %w", err)
	}

	return configData, nil

}

// downloadConfigFromURL downloads the VPN client configuration from the provided URL
func (i *Installer) downloadConfigFromURL(urlStr string) (string, error) {
	// Validate URL to prevent SSRF attacks
	parsedURL, err := url.Parse(urlStr)
	if err != nil {
		return "", fmt.Errorf("invalid URL: %w", err)
	}

	// Only allow HTTPS URLs for security
	if parsedURL.Scheme != "https" {
		return "", fmt.Errorf("only HTTPS URLs are allowed, got: %s", parsedURL.Scheme)
	}

	// Validate that this is an Azure Blob Storage URL for VPN configuration
	host := strings.ToLower(parsedURL.Host)
	if !strings.HasSuffix(host, ".blob.core.windows.net") {
		return "", fmt.Errorf("URL must be from Azure Blob Storage: %s", parsedURL.Host)
	}

	// Create temporary file for ZIP download
	tempZipFile, err := os.CreateTemp("", tempVPNZipPattern)
	if err != nil {
		return "", fmt.Errorf("failed to create temporary ZIP file: %w", err)
	}
	tempZipPath := tempZipFile.Name()
	_ = tempZipFile.Close() // Close file handle so utils.DownloadFile can write to it

	defer func() {
		if err := utils.RunCleanupCommand(tempZipPath); err != nil {
			i.logger.Warnf("Failed to clean up temp ZIP file %s: %v", tempZipPath, err)
		}
	}()

	// Download the ZIP file using utils.DownloadFile
	i.logger.Info("Downloading VPN configuration ZIP file...")
	if err := utils.DownloadFile(urlStr, tempZipPath); err != nil {
		return "", fmt.Errorf("failed to download VPN config ZIP: %w", err)
	}

	// Extract and return the OpenVPN configuration
	return i.extractOpenVPNConfig(tempZipPath)
}

// extractOpenVPNConfig extracts the OpenVPN configuration from a ZIP file
func (i *Installer) extractOpenVPNConfig(zipPath string) (string, error) {
	// Open the ZIP file
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", fmt.Errorf("failed to open VPN config ZIP: %w", err)
	}
	defer func() { _ = reader.Close() }()

	// Look for any .ovpn file in the ZIP (handle different path separators)
	i.logger.Info("Examining ZIP contents:")
	for _, file := range reader.File {
		i.logger.Infof("  File: %s", file.Name)
		fileName := strings.ToLower(file.Name)
		// Check for .ovpn files in OpenVPN directory (handle both / and \ separators)
		if strings.HasSuffix(fileName, ".ovpn") &&
			(strings.Contains(fileName, "openvpn/") || strings.Contains(fileName, "openvpn\\")) {
			// Extract and read the config file with size limits
			fileReader, err := file.Open()
			if err != nil {
				return "", fmt.Errorf("failed to open OpenVPN config: %w", err)
			}
			defer func() { _ = fileReader.Close() }()

			// Add size limit of 1MB (config files are typically small)
			const maxFileSize = 1024 * 1024 // 1MB
			limitedReader := io.LimitReader(fileReader, maxFileSize)

			configData, err := io.ReadAll(limitedReader)
			if err != nil {
				return "", fmt.Errorf("failed to read OpenVPN config: %w", err)
			}

			i.logger.Info("VPN configuration extracted successfully")
			return string(configData), nil
		}
	}

	return "", fmt.Errorf("OpenVPN configuration file (.ovpn) not found in ZIP")
}
