#!/bin/bash
# private-install.sh - Called by: aks-flex-node private-join
# Join local node to Private AKS Cluster via Gateway
#
# Usage:
#   sudo ./aks-flex-node private-join --aks-resource-id "/subscriptions/.../managedClusters/xxx"

set -euo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color

# Configuration
GATEWAY_NAME="wg-gateway"
GATEWAY_SUBNET_NAME="wg-subnet"
GATEWAY_SUBNET_PREFIX="10.0.100.0/24"
GATEWAY_VPN_NETWORK="172.16.0.0/24"
GATEWAY_VPN_IP="172.16.0.1"
GATEWAY_VM_SIZE="Standard_D2s_v3"
GATEWAY_PORT="51820"
NETWORK_INTERFACE="wg-aks"
# Handle sudo: use original user's home directory
if [[ -n "${SUDO_USER:-}" ]]; then
    REAL_HOME=$(getent passwd "$SUDO_USER" | cut -d: -f6)
else
    REAL_HOME="$HOME"
fi
SSH_KEY_PATH="${REAL_HOME}/.ssh/id_rsa_wg_gateway"
VERBOSE=false

# Cleanup function for Ctrl+C
cleanup_on_exit() {
    echo ""
    log_warning "Interrupted! Cleaning up..."
    sudo pkill -f "aks-flex-node agent" 2>/dev/null || true
    exit 1
}

# Trap Ctrl+C and other termination signals
trap cleanup_on_exit SIGINT SIGTERM

# Functions
log_info() {
    echo -e "${BLUE}INFO:${NC} $1"
}

log_success() {
    echo -e "${GREEN}SUCCESS:${NC} $1"
}

log_warning() {
    echo -e "${YELLOW}WARNING:${NC} $1"
}

log_error() {
    echo -e "${RED}ERROR:${NC} $1"
}

log_verbose() {
    if [[ "$VERBOSE" == "true" ]]; then
        echo -e "${BLUE}VERBOSE:${NC} $1"
    fi
}

parse_args() {
    while [[ $# -gt 0 ]]; do
        case $1 in
            --aks-resource-id)
                AKS_RESOURCE_ID="$2"
                shift 2
                ;;
            --gateway-name)
                GATEWAY_NAME="$2"
                shift 2
                ;;
            --gateway-subnet)
                GATEWAY_SUBNET_PREFIX="$2"
                shift 2
                ;;
            --gateway-vm-size)
                GATEWAY_VM_SIZE="$2"
                shift 2
                ;;
            --verbose)
                VERBOSE=true
                shift
                ;;
            *)
                log_error "Unknown argument: $1"
                exit 1
                ;;
        esac
    done

    # Validate required arguments
    if [[ -z "${AKS_RESOURCE_ID:-}" ]]; then
        log_error "Missing required argument: --aks-resource-id"
        exit 1
    fi

    # Parse AKS Resource ID
    parse_aks_resource_id
}

parse_aks_resource_id() {
    # Format: /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.ContainerService/managedClusters/{name}
    # Normalize: Azure CLI sometimes returns lowercase 'resourcegroups', but Go code expects 'resourceGroups'
    AKS_RESOURCE_ID=$(echo "$AKS_RESOURCE_ID" | sed 's|/resourcegroups/|/resourceGroups/|g')

    SUBSCRIPTION_ID=$(echo "$AKS_RESOURCE_ID" | cut -d'/' -f3)
    RESOURCE_GROUP=$(echo "$AKS_RESOURCE_ID" | cut -d'/' -f5)
    AKS_CLUSTER_NAME=$(echo "$AKS_RESOURCE_ID" | cut -d'/' -f9)

    if [[ -z "$SUBSCRIPTION_ID" || -z "$RESOURCE_GROUP" || -z "$AKS_CLUSTER_NAME" ]]; then
        log_error "Invalid AKS Resource ID format"
        exit 1
    fi

    log_verbose "Subscription ID: $SUBSCRIPTION_ID"
    log_verbose "Resource Group: $RESOURCE_GROUP"
    log_verbose "AKS Cluster Name: $AKS_CLUSTER_NAME"
}

# Phase 1: Environment Check
phase1_environment_check() {
    # Clean up old kube cache to avoid stale tokens
    log_info "Cleaning up old kube cache..."
    rm -rf /root/.kube/cache 2>/dev/null || true
    rm -rf "${REAL_HOME}/.kube/cache" 2>/dev/null || true
    log_success "Kube cache cleaned"

    # Check Azure CLI is installed
    log_info "Checking Azure CLI..."
    if ! command -v az &>/dev/null; then
        log_error "Azure CLI not installed. Please install: curl -sL https://aka.ms/InstallAzureCLIDeb | sudo bash"
        exit 1
    fi
    log_success "Azure CLI installed"

    # Check Azure CLI login status
    log_info "Checking Azure CLI login status..."
    if ! az account show &>/dev/null; then
        log_error "Azure CLI not logged in, please run 'az login' first"
        exit 1
    fi
    log_success "Azure CLI logged in"

    # Check if token is valid
    if ! az account get-access-token --only-show-errors &>/dev/null; then
        log_warning "Azure token expired or invalid, re-authenticating..."
        az login
    fi

    # Set subscription
    log_info "Setting subscription: $SUBSCRIPTION_ID"
    az account set --subscription "$SUBSCRIPTION_ID"
    log_success "Subscription set successfully"

    # Get Tenant ID
    TENANT_ID=$(az account show --query tenantId -o tsv)
    log_verbose "Tenant ID: $TENANT_ID"

    # Verify AKS cluster exists
    log_info "Verifying AKS cluster: $AKS_CLUSTER_NAME"
    if ! az aks show --resource-group "$RESOURCE_GROUP" --name "$AKS_CLUSTER_NAME" &>/dev/null; then
        log_error "AKS cluster '$AKS_CLUSTER_NAME' not found"
        exit 1
    fi

    # Check AAD and RBAC
    log_info "Checking AKS cluster AAD and RBAC configuration..."
    AAD_ENABLED=$(az aks show --resource-group "$RESOURCE_GROUP" --name "$AKS_CLUSTER_NAME" \
        --query "aadProfile.managed" -o tsv 2>/dev/null || echo "false")
    RBAC_ENABLED=$(az aks show --resource-group "$RESOURCE_GROUP" --name "$AKS_CLUSTER_NAME" \
        --query "aadProfile.enableAzureRbac" -o tsv 2>/dev/null || echo "false")

    if [[ "$AAD_ENABLED" != "true" ]]; then
        log_error "AKS cluster AAD not enabled, please enable: az aks update --enable-aad"
        exit 1
    fi
    if [[ "$RBAC_ENABLED" != "true" ]]; then
        log_error "AKS cluster Azure RBAC not enabled, please enable: az aks update --enable-azure-rbac"
        exit 1
    fi
    log_success "AKS cluster AAD and RBAC enabled"


    # Get AKS VNet info
    log_info "Getting AKS VNet info..."
    AKS_NODE_RG=$(az aks show --resource-group "$RESOURCE_GROUP" --name "$AKS_CLUSTER_NAME" \
        --query "nodeResourceGroup" -o tsv)

    # Get VNet info from VMSS
    VMSS_NAME=$(az vmss list --resource-group "$AKS_NODE_RG" --query "[0].name" -o tsv)
    if [[ -z "$VMSS_NAME" ]]; then
        log_error "Cannot find AKS node VMSS"
        exit 1
    fi

    VNET_SUBNET_ID=$(az vmss show --resource-group "$AKS_NODE_RG" --name "$VMSS_NAME" \
        --query "virtualMachineProfile.networkProfile.networkInterfaceConfigurations[0].ipConfigurations[0].subnet.id" -o tsv)

    VNET_NAME=$(echo "$VNET_SUBNET_ID" | cut -d'/' -f9)
    VNET_RG=$(echo "$VNET_SUBNET_ID" | cut -d'/' -f5)

    log_success "VNet: $VNET_NAME (Resource Group: $VNET_RG)"

    # Get Location
    LOCATION=$(az aks show --resource-group "$RESOURCE_GROUP" --name "$AKS_CLUSTER_NAME" \
        --query "location" -o tsv)
    log_verbose "Location: $LOCATION"

    # Check local dependencies
    log_info "Checking local dependencies..."

    if ! command -v wg &>/dev/null; then
        log_info "Installing VPN tools..."
        sudo apt-get update && sudo apt-get install -y wireguard-tools
    fi
    log_success "VPN tools installed"

    if ! command -v jq &>/dev/null; then
        log_info "Installing jq..."
        sudo apt-get install -y jq
    fi
    log_success "jq installed"

    # Install kubectl and kubelogin
    if ! command -v kubectl &>/dev/null || ! command -v kubelogin &>/dev/null; then
        log_info "Installing kubectl and kubelogin..."
        az aks install-cli --install-location /usr/local/bin/kubectl --kubelogin-install-location /usr/local/bin/kubelogin
        chmod +x /usr/local/bin/kubectl /usr/local/bin/kubelogin
    fi
    # Verify installation
    if ! command -v kubectl &>/dev/null; then
        log_error "kubectl installation failed"
        exit 1
    fi
    if ! command -v kubelogin &>/dev/null; then
        log_error "kubelogin installation failed"
        exit 1
    fi
    log_success "kubectl and kubelogin installed"

    # Install Azure CLI connectedmachine extension
    if ! az extension show --name connectedmachine &>/dev/null; then
        log_info "Installing Azure CLI connectedmachine extension..."
        az config set extension.dynamic_install_allow_preview=true --only-show-errors 2>/dev/null || true
        az extension add --name connectedmachine --allow-preview true --only-show-errors
    fi
    log_success "Azure CLI extensions ready"
}

# Phase 2: Gateway Setup
phase2_gateway_setup() {
    # Check if Gateway exists
    log_info "Checking if Gateway exists..."
    if az vm show --resource-group "$RESOURCE_GROUP" --name "$GATEWAY_NAME" &>/dev/null; then
        log_info "Gateway exists, reusing"
        GATEWAY_EXISTS=true

        # Get Public IP
        WG_PUBLIC_IP=$(az vm list-ip-addresses --resource-group "$RESOURCE_GROUP" --name "$GATEWAY_NAME" \
            --query "[0].virtualMachine.network.publicIpAddresses[0].ipAddress" -o tsv)
        log_success "Gateway Public IP: $WG_PUBLIC_IP"
    else
        log_info "Gateway not found, creating new one"
        GATEWAY_EXISTS=false
        create_gateway_infrastructure
    fi

    # Ensure SSH key exists
    ensure_ssh_key

    # Add SSH key to Gateway (idempotent, works for both new and existing Gateway)
    log_info "Adding SSH key to Gateway..."
    az vm user update \
        --resource-group "$RESOURCE_GROUP" \
        --name "$GATEWAY_NAME" \
        --username azureuser \
        --ssh-key-value "$(cat ${SSH_KEY_PATH}.pub)" \
        --output none
    log_success "SSH key added to Gateway"

    # Wait for VM ready and get server info
    wait_for_vm_ready
    get_server_info
}

create_gateway_infrastructure() {
    # Create Gateway Subnet
    log_info "Checking/creating Gateway subnet..."
    if ! az network vnet subnet show --resource-group "$VNET_RG" --vnet-name "$VNET_NAME" \
        --name "$GATEWAY_SUBNET_NAME" &>/dev/null; then
        az network vnet subnet create \
            --resource-group "$VNET_RG" \
            --vnet-name "$VNET_NAME" \
            --name "$GATEWAY_SUBNET_NAME" \
            --address-prefixes "$GATEWAY_SUBNET_PREFIX"
        log_success "Subnet $GATEWAY_SUBNET_NAME created"
    else
        log_info "Subnet $GATEWAY_SUBNET_NAME already exists"
    fi

    # Create NSG
    log_info "Checking/creating NSG..."
    NSG_NAME="${GATEWAY_NAME}-nsg"
    if ! az network nsg show --resource-group "$RESOURCE_GROUP" --name "$NSG_NAME" &>/dev/null; then
        az network nsg create --resource-group "$RESOURCE_GROUP" --name "$NSG_NAME"

        # Add SSH rule (priority 100 to override NRMS-Rule-106 which denies SSH from Internet at priority 106)
        az network nsg rule create \
            --resource-group "$RESOURCE_GROUP" \
            --nsg-name "$NSG_NAME" \
            --name allow-ssh \
            --priority 100 \
            --destination-port-ranges 22 \
            --protocol Tcp \
            --access Allow

        # Add VPN rule
        az network nsg rule create \
            --resource-group "$RESOURCE_GROUP" \
            --nsg-name "$NSG_NAME" \
            --name allow-wireguard \
            --priority 200 \
            --destination-port-ranges "$GATEWAY_PORT" \
            --protocol Udp \
            --access Allow

        log_success "NSG $NSG_NAME created"
    else
        log_info "NSG $NSG_NAME already exists"
    fi

    # Create Public IP
    log_info "Checking/creating Public IP..."
    PIP_NAME="${GATEWAY_NAME}-pip"
    if ! az network public-ip show --resource-group "$RESOURCE_GROUP" --name "$PIP_NAME" &>/dev/null; then
        az network public-ip create \
            --resource-group "$RESOURCE_GROUP" \
            --name "$PIP_NAME" \
            --sku Standard \
            --allocation-method Static
        log_success "Public IP $PIP_NAME created"
    else
        log_info "Public IP $PIP_NAME already exists"
    fi

    # Generate SSH key
    ensure_ssh_key

    # Create VM
    log_info "Creating Gateway..."
    az vm create \
        --resource-group "$RESOURCE_GROUP" \
        --name "$GATEWAY_NAME" \
        --image Ubuntu2204 \
        --size "$GATEWAY_VM_SIZE" \
        --vnet-name "$VNET_NAME" \
        --subnet "$GATEWAY_SUBNET_NAME" \
        --nsg "$NSG_NAME" \
        --public-ip-address "$PIP_NAME" \
        --admin-username azureuser \
        --ssh-key-values "${SSH_KEY_PATH}.pub" \
        --zone 1

    # Get Public IP
    WG_PUBLIC_IP=$(az network public-ip show --resource-group "$RESOURCE_GROUP" --name "$PIP_NAME" \
        --query ipAddress -o tsv)
    log_success "Gateway created, Public IP: $WG_PUBLIC_IP"

    # Wait for new VM to boot up
    log_info "Waiting 120 seconds for VM to boot up..."
    sleep 120
}

ensure_ssh_key() {
    if [[ ! -f "$SSH_KEY_PATH" ]]; then
        log_info "Generating SSH key..."
        ssh-keygen -t rsa -b 4096 -f "$SSH_KEY_PATH" -N ""
        # Fix ownership if running with sudo (so user can SSH without sudo)
        if [[ -n "${SUDO_USER:-}" ]]; then
            chown "$SUDO_USER:$SUDO_USER" "$SSH_KEY_PATH" "${SSH_KEY_PATH}.pub"
        fi
        log_success "SSH key generated: $SSH_KEY_PATH"
    else
        log_info "SSH key already exists: $SSH_KEY_PATH"
    fi
}

wait_for_vm_ready() {
    log_info "Checking VM SSH connectivity..."

    # First quick check
    if ssh -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o ConnectTimeout=10 -i "$SSH_KEY_PATH" \
        azureuser@"$WG_PUBLIC_IP" "echo ready" &>/dev/null; then
        log_success "VM SSH connection ready"
        return 0
    fi

    # SSH failed, restart VM if it's an existing VM
    if [[ "$GATEWAY_EXISTS" == "true" ]]; then
        log_warning "SSH connection failed, restarting VM..."
        az vm restart --resource-group "$RESOURCE_GROUP" --name "$GATEWAY_NAME" --no-wait
        log_info "Waiting 120 seconds for VM to restart..."
        sleep 120
    fi

    # Wait for SSH with retries
    log_info "Waiting for VM to be ready..."
    local max_attempts=18
    local attempt=0

    while [[ $attempt -lt $max_attempts ]]; do
        if ssh -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o ConnectTimeout=5 -i "$SSH_KEY_PATH" \
            azureuser@"$WG_PUBLIC_IP" "echo ready" &>/dev/null; then
            log_success "VM SSH connection ready"
            return 0
        fi
        attempt=$((attempt + 1))
        log_verbose "Waiting for SSH... ($attempt/$max_attempts)"
        sleep 10
    done

    log_error "VM SSH connection timeout"
    exit 1
}

get_server_info() {
    log_info "Getting/configuring Gateway server..."

    # Check if networking is already installed
    if ! ssh -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -i "$SSH_KEY_PATH" azureuser@"$WG_PUBLIC_IP" "command -v wg" &>/dev/null; then
        log_info "Installing and configuring networking on Gateway..."
        install_wireguard_server
    else
        log_info "Networking already installed"
    fi

    # Get server public key
    SERVER_PUBLIC_KEY=$(ssh -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -i "$SSH_KEY_PATH" azureuser@"$WG_PUBLIC_IP" \
        "sudo cat /etc/wireguard/server_public.key 2>/dev/null || echo ''")

    if [[ -z "$SERVER_PUBLIC_KEY" ]]; then
        log_info "Server key not found, reconfiguring..."
        install_wireguard_server
        SERVER_PUBLIC_KEY=$(ssh -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -i "$SSH_KEY_PATH" azureuser@"$WG_PUBLIC_IP" \
            "sudo cat /etc/wireguard/server_public.key")
    fi

    log_success "Server public key retrieved"

    # Get existing peer count
    EXISTING_PEERS=$(ssh -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -i "$SSH_KEY_PATH" azureuser@"$WG_PUBLIC_IP" \
        "sudo wg show wg0 peers 2>/dev/null | wc -l || echo 0")
    log_verbose "Existing peer count: $EXISTING_PEERS"

    # Calculate client IP
    CLIENT_IP_SUFFIX=$((EXISTING_PEERS + 2))
    CLIENT_VPN_IP="172.16.0.${CLIENT_IP_SUFFIX}"
    log_success "Assigned client VPN IP: $CLIENT_VPN_IP"
}

install_wireguard_server() {
    ssh -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -i "$SSH_KEY_PATH" azureuser@"$WG_PUBLIC_IP" << 'REMOTE_SCRIPT'
set -e

# Install networking
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

# Start networking
sudo systemctl enable wg-quick@wg0
sudo systemctl start wg-quick@wg0 || sudo systemctl restart wg-quick@wg0

echo "Gateway server configuration complete"
REMOTE_SCRIPT
}

# Phase 3: Client Configuration
phase3_client_setup() {
    # Generate client key pair
    log_info "Generating client key pair..."
    CLIENT_PRIVATE_KEY=$(wg genkey)
    CLIENT_PUBLIC_KEY=$(echo "$CLIENT_PRIVATE_KEY" | wg pubkey)
    log_success "Client key pair generated"

    # Create Gateway client configuration
    log_info "Creating Gateway client configuration..."
    sudo tee /etc/wireguard/${NETWORK_INTERFACE}.conf > /dev/null << EOF
[Interface]
PrivateKey = ${CLIENT_PRIVATE_KEY}
Address = ${CLIENT_VPN_IP}/24

[Peer]
PublicKey = ${SERVER_PUBLIC_KEY}
Endpoint = ${WG_PUBLIC_IP}:${GATEWAY_PORT}
AllowedIPs = 10.0.0.0/8, 172.16.0.0/24
PersistentKeepalive = 25
EOF
    sudo chmod 600 /etc/wireguard/${NETWORK_INTERFACE}.conf
    log_success "Client configuration created"

    # Add client peer to server
    log_info "Adding client peer to server..."
    ssh -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -i "$SSH_KEY_PATH" azureuser@"$WG_PUBLIC_IP" \
        "sudo wg set wg0 peer '${CLIENT_PUBLIC_KEY}' allowed-ips ${CLIENT_VPN_IP}/32"

    # Persist configuration
    ssh -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -i "$SSH_KEY_PATH" azureuser@"$WG_PUBLIC_IP" "sudo wg-quick save wg0"
    log_success "Client peer added"

    # Start networking connection
    log_info "Starting networking connection..."
    sudo wg-quick down "$NETWORK_INTERFACE" 2>/dev/null || true
    sudo wg-quick up "$NETWORK_INTERFACE"

    # Verify connection
    sleep 3
    if ping -c 1 -W 3 "$GATEWAY_VPN_IP" &>/dev/null; then
        log_success "Networking connected, can ping Gateway ($GATEWAY_VPN_IP)"
    else
        log_error "Networking connection failed, cannot ping Gateway"
        exit 1
    fi
}

# Phase 4: Node Join
phase4_node_join() {
    # Get API Server private FQDN
    log_info "Getting AKS API Server address..."
    API_SERVER_FQDN=$(az aks show --resource-group "$RESOURCE_GROUP" --name "$AKS_CLUSTER_NAME" \
        --query "privateFqdn" -o tsv)
    log_verbose "API Server FQDN: $API_SERVER_FQDN"

    # Resolve private DNS through Gateway
    log_info "Resolving API Server private IP..."
    API_SERVER_IP=$(ssh -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -i "$SSH_KEY_PATH" azureuser@"$WG_PUBLIC_IP" \
        "nslookup $API_SERVER_FQDN | grep -A1 'Name:' | grep 'Address:' | awk '{print \$2}'" 2>/dev/null || echo "")

    if [[ -z "$API_SERVER_IP" ]]; then
        log_error "Cannot resolve API Server private IP"
        exit 1
    fi
    log_success "API Server IP: $API_SERVER_IP"

    # Add hosts entry
    log_info "Adding hosts entry..."
    if ! grep -q "$API_SERVER_FQDN" /etc/hosts; then
        echo "$API_SERVER_IP $API_SERVER_FQDN" | sudo tee -a /etc/hosts
        log_success "Hosts entry added"
    else
        log_info "Hosts entry already exists"
    fi

    # Disable swap
    log_info "Disabling swap..."
    sudo swapoff -a
    log_success "Swap disabled"

    # Install Azure Arc agent (required for aks-flex-node)
    log_info "Checking Azure Arc agent..."
    if ! command -v azcmagent &>/dev/null; then
        log_info "Installing Azure Arc agent..."
        # Clean up any existing package state to avoid conflicts
        sudo dpkg --purge azcmagent 2>/dev/null || true

        local temp_dir
        temp_dir=$(mktemp -d)

        curl -L -o "$temp_dir/install_linux_azcmagent.sh" https://gbl.his.arc.azure.com/azcmagent-linux
        chmod +x "$temp_dir/install_linux_azcmagent.sh"
        sudo bash "$temp_dir/install_linux_azcmagent.sh"
        rm -rf "$temp_dir"

        log_success "Azure Arc agent installed"
    else
        log_info "Azure Arc agent already installed"
    fi

    # Get AKS credentials (save to root's kubeconfig for consistency with sudo az login)
    log_info "Getting AKS credentials..."
    mkdir -p /root/.kube
    az aks get-credentials --resource-group "$RESOURCE_GROUP" --name "$AKS_CLUSTER_NAME" \
        --overwrite-existing --file /root/.kube/config

    # Convert kubeconfig to use Azure CLI auth (for AAD + Azure RBAC)
    log_info "Converting kubeconfig for Azure CLI auth..."
    kubelogin convert-kubeconfig -l azurecli --kubeconfig /root/.kube/config
    log_success "Kubeconfig ready (saved to /root/.kube/config)"

    # Generate config.json
    log_info "Generating aks-flex-node configuration..."
    SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    PROJECT_ROOT="${SCRIPT_DIR}/../.."
    CONFIG_FILE="${PROJECT_ROOT}/config.json"

    cat > "$CONFIG_FILE" << EOF
{
  "azure": {
    "subscriptionId": "${SUBSCRIPTION_ID}",
    "tenantId": "${TENANT_ID}",
    "targetCluster": {
      "resourceId": "${AKS_RESOURCE_ID}",
      "location": "${LOCATION}"
    },
    "arc": {
      "resourceGroup": "${RESOURCE_GROUP}",
      "location": "${LOCATION}"
    }
  },
  "network": {
    "mode": "wireguard",
    "wireguard": {
      "serverEndpoint": "${WG_PUBLIC_IP}:${GATEWAY_PORT}",
      "serverPublicKey": "${SERVER_PUBLIC_KEY}",
      "clientAddress": "${CLIENT_VPN_IP}/24",
      "allowedIPs": ["10.0.0.0/8", "172.16.0.0/24"],
      "persistentKeepalive": 25,
      "testEndpoint": "${API_SERVER_IP}:443"
    }
  },
  "kubernetes": {
    "version": "1.29.0"
  },
  "containerd": {
    "version": "1.7.11",
    "pauseImage": "mcr.microsoft.com/oss/kubernetes/pause:3.6"
  },
  "agent": {
    "logLevel": "info",
    "logDir": "/var/log/aks-flex-node"
  }
}
EOF
    log_success "Config file generated: $CONFIG_FILE"

    # Run aks-flex-node
    log_info "Running aks-flex-node agent..."
    cd "${PROJECT_ROOT}"

    # Build if needed
    if [[ ! -f "./aks-flex-node" ]]; then
        log_info "Building aks-flex-node..."
        go build -o aks-flex-node .
    fi

    # Kill any existing aks-flex-node agent process
    log_info "Stopping any existing aks-flex-node agent..."
    sudo pkill -f "aks-flex-node agent" 2>/dev/null || true
    sleep 2

    # Create log directory
    sudo mkdir -p /var/log/aks-flex-node

    # Run agent in background
    LOG_FILE="/var/log/aks-flex-node/agent.log"
    sudo bash -c "./aks-flex-node agent --config '$CONFIG_FILE' > '$LOG_FILE' 2>&1" &
    AGENT_PID=$!
    log_info "Agent started in background (PID: $AGENT_PID)"
    # Wait for bootstrap to complete (check log file, minimal output)
    log_info "Waiting for bootstrap to complete (may take 2-3 minutes)..."
    log_info "View details: sudo tail -f $LOG_FILE"

    local max_wait=300
    local waited=0
    local bootstrap_success=false
    local bootstrap_failed=false

    # Simple progress indicator
    printf "       "
    while [[ $waited -lt $max_wait ]]; do
        # Check success/failure
        if sudo grep -q "bootstrap completed successfully" "$LOG_FILE" </dev/null 2>/dev/null; then
            bootstrap_success=true
            break
        fi
        if sudo grep -q "Bootstrap failed\|bootstrap failed" "$LOG_FILE" </dev/null 2>/dev/null; then
            bootstrap_failed=true
            break
        fi
        printf "."
        sleep 5
        waited=$((waited + 5))
    done
    echo ""

    if [[ "$bootstrap_failed" == "true" ]]; then
        log_error "Bootstrap failed. Check: sudo tail -50 $LOG_FILE"
        exit 1
    fi

    if [[ "$bootstrap_success" == "true" ]]; then
        log_success "Bootstrap completed"
    else
        log_error "Timeout. Check: sudo tail -50 $LOG_FILE"
        exit 1
    fi

    # Wait for RBAC propagation (simple dots)
    printf "       "
    for i in {1..3}; do
        printf "."
        sleep 5
    done
    echo ""
    log_success "Node join completed"
}

# Phase 5: Verification
phase5_verification() {
    NODE_NAME=$(hostname | tr '[:upper:]' '[:lower:]')

    # Check node status (simple dots)
    log_info "Waiting for node ready..."
    printf "       "
    local max_attempts=30
    local attempt=0

    while [[ $attempt -lt $max_attempts ]]; do
        NODE_STATUS=$(kubectl --kubeconfig /root/.kube/config get node "$NODE_NAME" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "")
        if [[ "$NODE_STATUS" == "True" ]]; then
            break
        fi
        attempt=$((attempt + 1))
        printf "."
        sleep 5
    done
    echo ""

    if [[ "$NODE_STATUS" != "True" ]]; then
        log_error "Node not ready, timeout"
        exit 1
    fi

    log_success "Node $NODE_NAME is Ready"
    echo ""
    printf "${GREEN}========================================${NC}\n"
    printf "${GREEN} Success! Edge Node joined Private AKS Cluster${NC}\n"
    printf "${GREEN}========================================${NC}\n"
    printf "\n"
    printf "Node info:\n"
    printf "  - Node name: %s\n" "$NODE_NAME"
    printf "  - VPN IP: %s\n" "$CLIENT_VPN_IP"
    printf "  - AKS cluster: %s\n" "$AKS_CLUSTER_NAME"
    printf "\n"
    printf "Cluster nodes:\n"
    kubectl --kubeconfig /root/.kube/config get nodes -o wide 2>&1
    printf "\n"
    printf "${YELLOW}Tips:${NC}\n"
    printf "  - Please try: sudo kubectl get nodes\n"
    printf "\n"
}

main() {
    echo -e "${GREEN}========================================${NC}"
    echo -e "${GREEN} Add Edge Node to Private AKS Cluster${NC}"
    echo -e "${GREEN}========================================${NC}"
    echo ""

    parse_args "$@"
    phase1_environment_check
    phase2_gateway_setup
    phase3_client_setup
    phase4_node_join
    phase5_verification
}

# Run main
main "$@"
