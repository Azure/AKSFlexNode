#!/bin/bash
# private-uninstall.sh - Called by: aks-flex-node private-leave
# Cleanup Private AKS Cluster Edge Node configuration
#
# Usage:
#   sudo ./aks-flex-node private-leave --mode=local                        # Keep Gateway
#   sudo ./aks-flex-node private-leave --mode=full --aks-resource-id "..." # Full cleanup

set -euo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Configuration
GATEWAY_NAME="wg-gateway"
GATEWAY_SUBNET_NAME="wg-subnet"
NETWORK_INTERFACE="wg-aks"
CLEANUP_MODE=""
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="${SCRIPT_DIR}/../.."

# Handle sudo: use original user's home directory for SSH keys
if [[ -n "${SUDO_USER:-}" ]]; then
    REAL_HOME=$(getent passwd "$SUDO_USER" | cut -d: -f6)
else
    REAL_HOME="$HOME"
fi
SSH_KEY_PATH="${REAL_HOME}/.ssh/id_rsa_wg_gateway"

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

parse_args() {
    while [[ $# -gt 0 ]]; do
        case $1 in
            --local)
                CLEANUP_MODE="local"
                shift
                ;;
            --full)
                CLEANUP_MODE="full"
                shift
                ;;
            --aks-resource-id)
                AKS_RESOURCE_ID="$2"
                shift 2
                ;;
            *)
                log_error "Unknown argument: $1"
                exit 1
                ;;
        esac
    done

    if [[ -z "$CLEANUP_MODE" ]]; then
        log_error "Please specify cleanup mode: --local or --full"
        exit 1
    fi

    if [[ "$CLEANUP_MODE" == "full" && -z "${AKS_RESOURCE_ID:-}" ]]; then
        log_error "--full mode requires --aks-resource-id"
        exit 1
    fi

    if [[ -n "${AKS_RESOURCE_ID:-}" ]]; then
        # Remove possible quotes and whitespace
        AKS_RESOURCE_ID=$(echo "$AKS_RESOURCE_ID" | tr -d '"' | tr -d "'" | xargs)
        SUBSCRIPTION_ID=$(echo "$AKS_RESOURCE_ID" | cut -d'/' -f3)
        RESOURCE_GROUP=$(echo "$AKS_RESOURCE_ID" | cut -d'/' -f5)
        AKS_CLUSTER_NAME=$(echo "$AKS_RESOURCE_ID" | cut -d'/' -f9)

        log_info "Parsed subscription ID: $SUBSCRIPTION_ID"
        log_info "Parsed resource group: $RESOURCE_GROUP"
        log_info "Parsed cluster name: $AKS_CLUSTER_NAME"
    fi
}

cleanup_local() {
    log_info "Performing local cleanup (keeping Gateway)..."

    NODE_NAME=$(hostname | tr '[:upper:]' '[:lower:]')

    # Get Gateway IP (before stopping networking)
    GATEWAY_PUBLIC_IP=""
    CLIENT_PRIVATE_KEY=""
    if [[ -f "/etc/wireguard/${NETWORK_INTERFACE}.conf" ]]; then
        GATEWAY_PUBLIC_IP=$(sudo cat /etc/wireguard/${NETWORK_INTERFACE}.conf 2>/dev/null | grep "Endpoint" | cut -d'=' -f2 | cut -d':' -f1 | tr -d ' ' || echo "")
        CLIENT_PRIVATE_KEY=$(sudo cat /etc/wireguard/${NETWORK_INTERFACE}.conf 2>/dev/null | grep "PrivateKey" | cut -d'=' -f2 | tr -d ' ' || echo "")
    fi

    # Remove node from cluster (while networking is still connected)
    if command -v kubectl &>/dev/null; then
        log_info "Removing node $NODE_NAME from cluster..."
        # Try root kubeconfig first, then user's kubeconfig
        if kubectl --kubeconfig /root/.kube/config delete node "$NODE_NAME" --ignore-not-found 2>&1; then
            log_success "Node removed from cluster"
        elif kubectl delete node "$NODE_NAME" --ignore-not-found 2>&1; then
            log_success "Node removed from cluster"
        else
            log_warning "Failed to remove node from cluster (may need manual cleanup: kubectl delete node $NODE_NAME)"
        fi
    fi

    # Stop any running aks-flex-node agent process
    log_info "Stopping aks-flex-node agent..."
    sudo pkill -f "aks-flex-node agent" 2>/dev/null || true
    sleep 2

    # Run aks-flex-node unbootstrap
    log_info "Running aks-flex-node unbootstrap..."
    CONFIG_FILE="${PROJECT_ROOT}/config.json"
    AKS_FLEX_NODE="${PROJECT_ROOT}/aks-flex-node"

    if [[ -f "$AKS_FLEX_NODE" && -f "$CONFIG_FILE" ]]; then
        sudo "$AKS_FLEX_NODE" unbootstrap --config "$CONFIG_FILE" || true
        log_success "aks-flex-node unbootstrap completed"
    else
        log_warning "aks-flex-node or config.json not found, manually stopping services..."
        sudo systemctl stop kubelet 2>/dev/null || true
        sudo systemctl disable kubelet 2>/dev/null || true
        sudo systemctl stop containerd 2>/dev/null || true
    fi

    # Remove Arc Agent and Azure resource
    log_info "Removing Arc Agent..."
    if command -v azcmagent &>/dev/null; then
        # First, delete Azure resource (requires az login)
        log_info "Deleting Arc machine from Azure..."
        ARC_RG=$(sudo azcmagent show 2>/dev/null | grep "Resource Group" | awk -F: '{print $2}' | xargs || echo "")
        if [[ -n "$ARC_RG" ]]; then
            az connectedmachine delete --resource-group "$ARC_RG" --name "$NODE_NAME" --yes 2>/dev/null || true
            log_success "Arc machine deleted from Azure"
        fi
        # Then disconnect locally
        sudo azcmagent disconnect --force-local-only 2>/dev/null || true
        sudo systemctl stop himdsd extd gcad arcproxyd 2>/dev/null || true
        sudo systemctl disable himdsd extd gcad arcproxyd 2>/dev/null || true
        if command -v apt &>/dev/null; then
            sudo apt remove azcmagent -y 2>/dev/null || true
        elif command -v yum &>/dev/null; then
            sudo yum remove azcmagent -y 2>/dev/null || true
        fi
        sudo rm -rf /var/opt/azcmagent /opt/azcmagent 2>/dev/null || true
        log_success "Arc Agent removed"
    else
        log_info "Arc Agent not found, skipping"
    fi

    # Remove client peer from Gateway
    if [[ -n "$GATEWAY_PUBLIC_IP" && -n "$CLIENT_PRIVATE_KEY" && -f "$SSH_KEY_PATH" ]]; then
        log_info "Removing client peer from Gateway..."
        CLIENT_PUBLIC_KEY=$(echo "$CLIENT_PRIVATE_KEY" | wg pubkey 2>/dev/null || echo "")
        if [[ -n "$CLIENT_PUBLIC_KEY" ]]; then
            ssh -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o ConnectTimeout=10 -i "$SSH_KEY_PATH" \
                azureuser@"$GATEWAY_PUBLIC_IP" \
                "sudo wg set wg0 peer '$CLIENT_PUBLIC_KEY' remove && sudo wg-quick save wg0" 2>/dev/null || true
            log_success "Client peer removed from Gateway"
        fi
    fi

    # Stop networking
    log_info "Stopping VPN connection..."
    sudo wg-quick down "$NETWORK_INTERFACE" 2>/dev/null || true
    log_success "VPN connection stopped"

    # Delete Gateway client configuration
    log_info "Deleting VPN client configuration..."
    sudo rm -f /etc/wireguard/${NETWORK_INTERFACE}.conf
    log_success "VPN client configuration deleted"

    # Clean up hosts entries
    log_info "Cleaning up hosts entries..."
    sudo sed -i '/privatelink.*azmk8s.io/d' /etc/hosts
    log_success "Hosts entries cleaned up"

    # Delete config.json
    log_info "Deleting config file..."
    rm -f "$CONFIG_FILE"

    echo ""
    log_success "Local cleanup completed!"
    echo ""
    echo "To rejoin cluster, run:"
    echo "  sudo ./aks-flex-node private-join --aks-resource-id \"...\""
}

cleanup_full() {
    log_info "Performing full cleanup..."

    NODE_NAME=$(hostname | tr '[:upper:]' '[:lower:]')

    # Get Gateway IP (before stopping networking)
    GATEWAY_PUBLIC_IP=""
    CLIENT_PRIVATE_KEY=""
    if [[ -f "/etc/wireguard/${NETWORK_INTERFACE}.conf" ]]; then
        GATEWAY_PUBLIC_IP=$(sudo cat /etc/wireguard/${NETWORK_INTERFACE}.conf 2>/dev/null | grep "Endpoint" | cut -d'=' -f2 | cut -d':' -f1 | tr -d ' ' || echo "")
        CLIENT_PRIVATE_KEY=$(sudo cat /etc/wireguard/${NETWORK_INTERFACE}.conf 2>/dev/null | grep "PrivateKey" | cut -d'=' -f2 | tr -d ' ' || echo "")
    fi

    # Remove node from cluster (while networking is still connected)
    if command -v kubectl &>/dev/null; then
        log_info "Removing node $NODE_NAME from cluster..."
        # Try root kubeconfig first, then user's kubeconfig
        if kubectl --kubeconfig /root/.kube/config delete node "$NODE_NAME" --ignore-not-found 2>&1; then
            log_success "Node removed from cluster"
        elif kubectl delete node "$NODE_NAME" --ignore-not-found 2>&1; then
            log_success "Node removed from cluster"
        else
            log_warning "Failed to remove node from cluster (may need manual cleanup: kubectl delete node $NODE_NAME)"
        fi
    fi

    # Stop any running aks-flex-node agent process
    log_info "Stopping aks-flex-node agent..."
    sudo pkill -f "aks-flex-node agent" 2>/dev/null || true
    sleep 2

    # Run aks-flex-node unbootstrap
    log_info "Running aks-flex-node unbootstrap..."
    CONFIG_FILE="${PROJECT_ROOT}/config.json"
    AKS_FLEX_NODE="${PROJECT_ROOT}/aks-flex-node"

    if [[ -f "$AKS_FLEX_NODE" && -f "$CONFIG_FILE" ]]; then
        sudo "$AKS_FLEX_NODE" unbootstrap --config "$CONFIG_FILE" || true
        log_success "aks-flex-node unbootstrap completed"
    else
        log_warning "aks-flex-node or config.json not found, skipping unbootstrap"
        # Manually stop services
        log_info "Manually stopping services..."
        sudo systemctl stop kubelet 2>/dev/null || true
        sudo systemctl disable kubelet 2>/dev/null || true
        sudo systemctl stop containerd 2>/dev/null || true
    fi

    # Remove Arc Agent and Azure resource
    log_info "Removing Arc Agent..."
    if command -v azcmagent &>/dev/null; then
        # First, delete Azure resource (requires az login)
        log_info "Deleting Arc machine from Azure..."
        ARC_RG=$(sudo azcmagent show 2>/dev/null | grep "Resource Group" | awk -F: '{print $2}' | xargs || echo "")
        if [[ -n "$ARC_RG" ]]; then
            az connectedmachine delete --resource-group "$ARC_RG" --name "$NODE_NAME" --yes 2>/dev/null || true
            log_success "Arc machine deleted from Azure"
        else
            # Fallback: try using the resource group from args
            az connectedmachine delete --resource-group "$RESOURCE_GROUP" --name "$NODE_NAME" --yes 2>/dev/null || true
        fi
        # Then disconnect locally
        sudo azcmagent disconnect --force-local-only 2>/dev/null || true
        sudo systemctl stop himdsd extd gcad arcproxyd 2>/dev/null || true
        sudo systemctl disable himdsd extd gcad arcproxyd 2>/dev/null || true
        # Remove Arc Agent package
        if command -v apt &>/dev/null; then
            sudo apt remove azcmagent -y 2>/dev/null || true
        elif command -v yum &>/dev/null; then
            sudo yum remove azcmagent -y 2>/dev/null || true
        fi
        # Clean up Arc Agent files
        sudo rm -rf /var/opt/azcmagent /opt/azcmagent 2>/dev/null || true
        log_success "Arc Agent removed"
    else
        log_info "Arc Agent not found, skipping"
    fi

    # Remove client peer from Gateway
    if [[ -n "$GATEWAY_PUBLIC_IP" && -n "$CLIENT_PRIVATE_KEY" && -f "$SSH_KEY_PATH" ]]; then
        log_info "Removing client peer from Gateway..."
        CLIENT_PUBLIC_KEY=$(echo "$CLIENT_PRIVATE_KEY" | wg pubkey 2>/dev/null || echo "")
        if [[ -n "$CLIENT_PUBLIC_KEY" ]]; then
            ssh -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o ConnectTimeout=10 -i "$SSH_KEY_PATH" \
                azureuser@"$GATEWAY_PUBLIC_IP" \
                "sudo wg set wg0 peer '$CLIENT_PUBLIC_KEY' remove && sudo wg-quick save wg0" 2>/dev/null || true
            log_success "Client peer removed from Gateway"
        fi
    fi

    # Stop networking
    log_info "Stopping networking..."
    sudo wg-quick down "$NETWORK_INTERFACE" 2>/dev/null || true
    log_success "Networking stopped"

    # Delete Gateway client configuration
    log_info "Deleting Gateway client configuration..."
    sudo rm -f /etc/wireguard/${NETWORK_INTERFACE}.conf
    log_success "Gateway client configuration deleted"

    # Clean up hosts entries
    log_info "Cleaning up hosts entries..."
    sudo sed -i '/privatelink.*azmk8s.io/d' /etc/hosts
    log_success "Hosts entries cleaned up"

    # Delete Azure resources
    log_info "Deleting Azure resources..."
    az account set --subscription "$SUBSCRIPTION_ID"

    # Delete Gateway (must complete before deleting NIC)
    log_info "Deleting Gateway..."
    if az vm show --resource-group "$RESOURCE_GROUP" --name "$GATEWAY_NAME" &>/dev/null; then
        az vm delete --resource-group "$RESOURCE_GROUP" --name "$GATEWAY_NAME" --yes --only-show-errors
        log_success "Gateway deleted"
    else
        log_info "Gateway not found, skipping"
    fi

    # Delete NIC
    NIC_NAME="${GATEWAY_NAME}VMNic"
    log_info "Deleting NIC..."
    if az network nic show --resource-group "$RESOURCE_GROUP" --name "$NIC_NAME" &>/dev/null; then
        az network nic delete --resource-group "$RESOURCE_GROUP" --name "$NIC_NAME" --only-show-errors
        log_success "NIC deleted"
    else
        log_info "NIC not found, skipping"
    fi

    # Delete Public IP
    PIP_NAME="${GATEWAY_NAME}-pip"
    log_info "Deleting Public IP..."
    if az network public-ip show --resource-group "$RESOURCE_GROUP" --name "$PIP_NAME" &>/dev/null; then
        az network public-ip delete --resource-group "$RESOURCE_GROUP" --name "$PIP_NAME" --only-show-errors
        log_success "Public IP deleted"
    else
        log_info "Public IP not found, skipping"
    fi

    # Delete NSG
    NSG_NAME="${GATEWAY_NAME}-nsg"
    log_info "Deleting NSG..."
    if az network nsg show --resource-group "$RESOURCE_GROUP" --name "$NSG_NAME" &>/dev/null; then
        az network nsg delete --resource-group "$RESOURCE_GROUP" --name "$NSG_NAME" --only-show-errors
        log_success "NSG deleted"
    else
        log_info "NSG not found, skipping"
    fi

    # Delete disks
    log_info "Deleting disks..."
    DISK_NAMES=$(az disk list --resource-group "$RESOURCE_GROUP" --query "[?contains(name, '${GATEWAY_NAME}')].name" -o tsv 2>/dev/null || echo "")
    for disk in $DISK_NAMES; do
        az disk delete --resource-group "$RESOURCE_GROUP" --name "$disk" --yes --only-show-errors || true
    done

    # Get VNet info and delete subnet
    log_info "Deleting Gateway subnet..."
    AKS_NODE_RG=$(az aks show --resource-group "$RESOURCE_GROUP" --name "$AKS_CLUSTER_NAME" \
        --query "nodeResourceGroup" -o tsv 2>/dev/null || echo "")

    if [[ -n "$AKS_NODE_RG" ]]; then
        VMSS_NAME=$(az vmss list --resource-group "$AKS_NODE_RG" --query "[0].name" -o tsv 2>/dev/null || echo "")
        if [[ -n "$VMSS_NAME" ]]; then
            VNET_SUBNET_ID=$(az vmss show --resource-group "$AKS_NODE_RG" --name "$VMSS_NAME" \
                --query "virtualMachineProfile.networkProfile.networkInterfaceConfigurations[0].ipConfigurations[0].subnet.id" -o tsv 2>/dev/null || echo "")
            if [[ -n "$VNET_SUBNET_ID" ]]; then
                VNET_NAME=$(echo "$VNET_SUBNET_ID" | cut -d'/' -f9)
                VNET_RG=$(echo "$VNET_SUBNET_ID" | cut -d'/' -f5)
                az network vnet subnet delete --resource-group "$VNET_RG" --vnet-name "$VNET_NAME" \
                    --name "$GATEWAY_SUBNET_NAME" 2>/dev/null || true
                log_success "Gateway subnet deleted"
            fi
        fi
    fi

    # Delete SSH keys
    log_info "Deleting SSH keys..."
    rm -f "$SSH_KEY_PATH" "${SSH_KEY_PATH}.pub"
    log_success "SSH keys deleted"

    # Delete config.json
    log_info "Deleting config file..."
    rm -f "$CONFIG_FILE"

    echo ""
    log_success "Full cleanup completed!"
    echo ""
    echo "All components and Azure resources have been removed."
    echo "The local machine is now clean."
}

main() {
    echo -e "${YELLOW}Remove Edge Node from Private AKS Cluster${NC}"
    echo -e "${YELLOW}=====================================${NC}"
    echo ""

    parse_args "$@"

    # Install Azure CLI connectedmachine extension if needed
    if ! az extension show --name connectedmachine &>/dev/null; then
        log_info "Installing Azure CLI connectedmachine extension..."
        az config set extension.dynamic_install_allow_preview=true --only-show-errors 2>/dev/null || true
        az extension add --name connectedmachine --allow-preview true --only-show-errors 2>/dev/null || true
    fi

    case "$CLEANUP_MODE" in
        local)
            cleanup_local
            ;;
        full)
            cleanup_full
            ;;
    esac
}

# Run main
main "$@"
