package privatecluster

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/sirupsen/logrus"

	"go.goms.io/aks/AKSFlexNode/pkg/utils"
)

// VPNClient provides VPN (WireGuard) operations.
type VPNClient struct {
	config VPNConfig
	logger *logrus.Logger
}

// NewVPNClient creates a new VPNClient instance.
func NewVPNClient(config VPNConfig, logger *logrus.Logger) *VPNClient {
	return &VPNClient{
		config: config,
		logger: logger,
	}
}

// GenerateKeyPair generates a WireGuard key pair and returns (privateKey, publicKey).
func (v *VPNClient) GenerateKeyPair(ctx context.Context) (string, string, error) {
	privateKeyRaw, err := utils.RunCommandWithOutputContext(ctx, "wg", "genkey")
	if err != nil {
		return "", "", fmt.Errorf("failed to generate VPN private key: %w", err)
	}
	privateKey := strings.TrimSpace(privateKeyRaw)

	cmd := exec.CommandContext(ctx, "wg", "pubkey") // #nosec G204 -- fixed wg command
	cmd.Stdin = strings.NewReader(privateKey)
	publicKeyBytes, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("failed to generate VPN public key: %w", err)
	}

	return privateKey, strings.TrimSpace(string(publicKeyBytes)), nil
}

// CreateClientConfig creates the client VPN configuration file.
func (v *VPNClient) CreateClientConfig(privateKey string, gatewayPort int) error {
	configPath := fmt.Sprintf("/etc/wireguard/%s.conf", v.config.NetworkInterface)

	config := fmt.Sprintf(`[Interface]
PrivateKey = %s
Address = %s/24

[Peer]
PublicKey = %s
Endpoint = %s:%d
AllowedIPs = 10.0.0.0/8, 172.16.0.0/24
PersistentKeepalive = 25
`, privateKey, v.config.ClientVPNIP, v.config.ServerPublicKey, v.config.ServerEndpoint, gatewayPort)

	if err := WriteFileContent(configPath, config, 0600); err != nil {
		return fmt.Errorf("failed to create VPN client config: %w", err)
	}

	return nil
}

// Start starts the VPN connection.
func (v *VPNClient) Start(ctx context.Context) error {
	_ = v.Stop(ctx)

	_, err := utils.RunCommandWithOutputContext(ctx, "wg-quick", "up", v.config.NetworkInterface)
	if err != nil {
		return fmt.Errorf("failed to start VPN: %w", err)
	}

	return nil
}

// Stop stops the VPN connection.
func (v *VPNClient) Stop(ctx context.Context) error {
	_, _ = utils.RunCommandWithOutputContext(ctx, "wg-quick", "down", v.config.NetworkInterface)
	return nil
}

// TestConnection tests VPN connectivity by pinging the gateway.
func (v *VPNClient) TestConnection(ctx context.Context) bool {
	return utils.RunCommandSilentContext(ctx, "ping", "-c", "1", "-W", "3", v.config.GatewayVPNIP)
}

// RemoveClientConfig removes the client VPN configuration file.
func (v *VPNClient) RemoveClientConfig() error {
	configPath := fmt.Sprintf("/etc/wireguard/%s.conf", v.config.NetworkInterface)
	if utils.FileExists(configPath) {
		cmd := exec.Command("rm", "-f", configPath) // #nosec G204 -- fixed rm command
		return cmd.Run()
	}
	return nil
}

// GetClientConfigInfo reads the current client config and returns Gateway IP and private key.
func (v *VPNClient) GetClientConfigInfo() (gatewayIP, privateKey string, err error) {
	configPath := fmt.Sprintf("/etc/wireguard/%s.conf", v.config.NetworkInterface)

	content, err := ReadFileContent(configPath)
	if err != nil {
		return "", "", fmt.Errorf("failed to read VPN config: %w", err)
	}

	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Endpoint") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				endpoint := strings.TrimSpace(parts[1])
				gatewayIP = strings.Split(endpoint, ":")[0]
			}
		}
		if strings.HasPrefix(line, "PrivateKey") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				privateKey = strings.TrimSpace(parts[1])
			}
		}
	}

	return gatewayIP, privateKey, nil
}

// GetPublicKeyFromPrivate derives public key from private key.
func (v *VPNClient) GetPublicKeyFromPrivate(ctx context.Context, privateKey string) (string, error) {
	cmd := exec.CommandContext(ctx, "wg", "pubkey") // #nosec G204 -- fixed wg command
	cmd.Stdin = strings.NewReader(privateKey)
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to derive public key: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// VPNServerManager manages VPN server on the Gateway.
type VPNServerManager struct {
	ssh    *SSHClient
	logger *logrus.Logger
}

// NewVPNServerManager creates a new VPNServerManager instance.
func NewVPNServerManager(ssh *SSHClient, logger *logrus.Logger) *VPNServerManager {
	return &VPNServerManager{
		ssh:    ssh,
		logger: logger,
	}
}

// IsInstalled checks if VPN software is installed on the server.
func (m *VPNServerManager) IsInstalled(ctx context.Context) bool {
	return m.ssh.CommandExists(ctx, "wg")
}

// Install installs and configures VPN server.
func (m *VPNServerManager) Install(ctx context.Context) error {
	script := `set -e

# Install WireGuard
sudo apt-get update
sudo apt-get install -y wireguard

# Generate key pair
sudo wg genkey | sudo tee /etc/wireguard/server_private.key | sudo wg pubkey | sudo tee /etc/wireguard/server_public.key
sudo chmod 600 /etc/wireguard/server_private.key

SERVER_PRIVATE_KEY=$(sudo cat /etc/wireguard/server_private.key)

# Create configuration
sudo tee /etc/wireguard/wg0.conf << EOF
[Interface]
PrivateKey = ${SERVER_PRIVATE_KEY}
Address = 172.16.0.1/24
ListenPort = 51820
PostUp = iptables -A FORWARD -i wg0 -j ACCEPT; iptables -t nat -A POSTROUTING -o eth0 -j MASQUERADE
PostDown = iptables -D FORWARD -i wg0 -j ACCEPT; iptables -t nat -D POSTROUTING -o eth0 -j MASQUERADE
EOF

# Enable IP forwarding
echo 'net.ipv4.ip_forward=1' | sudo tee -a /etc/sysctl.conf
sudo sysctl -p

# Start VPN service
sudo systemctl enable wg-quick@wg0
sudo systemctl start wg-quick@wg0 || sudo systemctl restart wg-quick@wg0

echo "VPN server configuration complete"
`
	return m.ssh.ExecuteScript(ctx, script)
}

// GetPublicKey retrieves the server's public key.
func (m *VPNServerManager) GetPublicKey(ctx context.Context) (string, error) {
	key, err := m.ssh.ReadRemoteFile(ctx, "/etc/wireguard/server_public.key")
	if err != nil || key == "" {
		return "", fmt.Errorf("failed to get server public key")
	}
	return strings.TrimSpace(key), nil
}

// GetPeerCount returns the number of existing peers.
func (m *VPNServerManager) GetPeerCount(ctx context.Context) (int, error) {
	output, err := m.ssh.Execute(ctx, "sudo wg show wg0 peers 2>/dev/null | wc -l || echo 0")
	if err != nil {
		return 0, nil // Default to 0 if error
	}
	count, _ := strconv.Atoi(strings.TrimSpace(output))
	return count, nil
}

// AddPeer adds a client peer to the server.
func (m *VPNServerManager) AddPeer(ctx context.Context, clientPublicKey, clientIP string) error {
	cmd := fmt.Sprintf("sudo wg set wg0 peer '%s' allowed-ips %s/32", clientPublicKey, clientIP)
	if _, err := m.ssh.Execute(ctx, cmd); err != nil {
		return fmt.Errorf("failed to add peer: %w", err)
	}

	if _, err := m.ssh.Execute(ctx, "sudo wg-quick save wg0"); err != nil {
		return fmt.Errorf("failed to save VPN config: %w", err)
	}

	return nil
}

// RemovePeer removes a client peer from the server.
func (m *VPNServerManager) RemovePeer(ctx context.Context, clientPublicKey string) error {
	cmd := fmt.Sprintf("sudo wg set wg0 peer '%s' remove && sudo wg-quick save wg0", clientPublicKey)
	_, _ = m.ssh.Execute(ctx, cmd)
	return nil
}

// ResolveDNS resolves a hostname through the Gateway.
func (m *VPNServerManager) ResolveDNS(ctx context.Context, hostname string) (string, error) {
	cmd := fmt.Sprintf("nslookup %s | grep -A1 'Name:' | grep 'Address:' | awk '{print $2}'", hostname)
	output, err := m.ssh.Execute(ctx, cmd)
	if err != nil || output == "" {
		return "", fmt.Errorf("failed to resolve %s through Gateway", hostname)
	}
	return strings.TrimSpace(output), nil
}

// InstallVPNTools installs VPN tools locally.
func InstallVPNTools(ctx context.Context, logger *logrus.Logger) error {
	if CommandExists("wg") {
		return nil
	}
	if _, err := utils.RunCommandWithOutputContext(ctx, "apt-get", "update"); err != nil {
		return err
	}
	_, err := utils.RunCommandWithOutputContext(ctx, "apt-get", "install", "-y", "wireguard-tools")
	return err
}
