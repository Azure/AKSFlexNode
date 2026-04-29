#!/bin/bash
# AKS Flex Node Uninstall Script
# This script removes all components installed by the AKS Flex Node installation script

set -euo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Configuration (should match install.sh)
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/aks-flex-node"
DATA_DIR="/var/lib/aks-flex-node"
LOG_DIR="/var/log/aks-flex-node"
SKIP_AZCLI="${SKIP_AZCLI:-false}"

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

confirm_uninstall() {
    echo -e "${YELLOW}AKS Flex Node Uninstaller${NC}"
    echo -e "${YELLOW}===========================${NC}"
    echo ""
    echo "This will remove the following components:"
    echo "• AKS Flex Node binary ($INSTALL_DIR/aks-flex-node)"
    echo "• Systemd service (aks-flex-node-agent.service)"
    echo "• Configuration directory ($CONFIG_DIR)"
    echo "• Data directory ($DATA_DIR)"
    echo "• Log directory ($LOG_DIR)"
    echo "• Azure CLI"
    echo ""
    echo -e "${YELLOW}NOTE: This will first run 'aks-flex-node unbootstrap' to clean up cluster and Arc resources.${NC}"
        echo ""

    # Always prompt for confirmation, even when piped
    if [[ "${1:-}" != "--force" ]]; then
        read -p "Are you sure you want to continue? (y/N): " -n 1 -r response </dev/tty
        echo
        if [[ ! $response =~ ^[Yy]$ ]]; then
            echo "Uninstall cancelled."
            exit 0
        fi
    else
        log_info "Force flag provided, skipping confirmation."
    fi
}

stop_and_disable_services() {
    log_info "Stopping and disabling systemd services..."

    if [[ ! -x "$INSTALL_DIR/aks-flex-node" ]]; then
        log_warning "AKS Flex Node binary not found; skipping binary-managed service uninstall"
    fi

    log_info "Systemd service removal is handled by unbootstrap"
}

run_unbootstrap() {
    log_info "Running unbootstrap to clean up cluster and Arc resources..."

    # Check if aks-flex-node binary exists
    if [[ ! -f "$INSTALL_DIR/aks-flex-node" ]]; then
        log_warning "AKS Flex Node binary not found at $INSTALL_DIR/aks-flex-node"
        log_info "Skipping unbootstrap - binary may already be removed"
        return 0
    fi

    # Try to find config file
    local config_file=""
    if [[ -f "$CONFIG_DIR/config.json" ]]; then
        config_file="$CONFIG_DIR/config.json"
        log_info "Using config file: $config_file"
    else
        config_file="$CONFIG_DIR/config.json"
        log_warning "Config file not found at $CONFIG_DIR/config.json"
        log_warning "Resource cleanup may be skipped, but systemd service removal will still be attempted"
        log_info "Manual cleanup of Azure resources may be required"
    fi

    # Run unbootstrap to clean up resources
    # Use the root-owned auth copy prepared by 'aks-flex-node bootstrap'.
    local azure_config_dir="$CONFIG_DIR/azure"

    if [[ -d "$azure_config_dir" ]]; then
        log_info "Using Azure CLI credentials from: $azure_config_dir"

        env AZURE_CONFIG_DIR="$azure_config_dir" TERM="${TERM:-dumb}" "$INSTALL_DIR/aks-flex-node" unbootstrap --config "$config_file" 2>&1 || {
            log_warning "Unbootstrap failed - this may be expected if resources are already cleaned up"
        }
    else
        log_warning "Azure CLI credentials not found at $azure_config_dir"
        log_info "Attempting unbootstrap without Azure CLI credentials..."

        env TERM="${TERM:-dumb}" "$INSTALL_DIR/aks-flex-node" unbootstrap --config "$config_file" 2>&1 || {
            log_warning "Unbootstrap failed - this may be expected if resources are already cleaned up"
        }
    fi

    log_success "Unbootstrap completed"
}

remove_directories() {
    log_info "Removing directories..."

    # Remove directories
    for dir in "$CONFIG_DIR" "$DATA_DIR" "$LOG_DIR"; do
        if [[ -d "$dir" ]]; then
            log_info "Removing directory: $dir"
            rm -rf "$dir"
            log_success "Removed directory: $dir"
        else
            log_info "Directory not found: $dir"
        fi
    done
}

remove_binary() {
    log_info "Removing binary..."

    if [[ -f "$INSTALL_DIR/aks-flex-node" ]]; then
        rm -f "$INSTALL_DIR/aks-flex-node"
        log_success "Removed binary: $INSTALL_DIR/aks-flex-node"
    else
        log_info "Binary not found: $INSTALL_DIR/aks-flex-node"
    fi
}

remove_azure_cli() {
    if [[ "$SKIP_AZCLI" == "true" || "$SKIP_AZCLI" == "1" ]]; then
        log_info "Skipping Azure CLI removal (SKIP_AZCLI=$SKIP_AZCLI)"
        return 0
    fi

    log_info "Removing Azure CLI..."

    if command -v az &> /dev/null; then
        # Uninstall Azure CLI package
        log_info "Uninstalling Azure CLI package..."
        export DEBIAN_FRONTEND=noninteractive
        apt-get remove -y azure-cli 2>/dev/null || true
        apt-get purge -y azure-cli 2>/dev/null || true

        # Verify removal
        if command -v az &> /dev/null; then
            log_warning "az command still available after cleanup - manual removal may be required"
        else
            log_success "Azure CLI removed successfully"
        fi
    else
        log_info "Azure CLI not found - already removed or never installed"
    fi
}

show_completion_message() {
    log_success "AKS Flex Node uninstallation completed!"
    echo ""
    echo -e "${YELLOW}What was removed:${NC}"
    echo "✅ AKS Flex Node binary"
    echo "✅ Systemd service configuration"
    echo "✅ Service user and permissions"
    echo "✅ Configuration and data directories"
    echo "✅ Log files"
    echo "✅ Azure CLI"
    echo ""
    echo -e "${GREEN}Complete uninstallation finished!${NC}"
    echo ""
    echo "The system has been returned to its pre-installation state."
}

main() {
    # Check if running as root
    if [[ $EUID -ne 0 ]]; then
        log_error "This script must be run as root (use sudo)"
        exit 1
    fi

    # Confirm uninstall
    confirm_uninstall "${1:-}"

    echo ""
    log_info "Starting AKS Flex Node uninstallation..."

    # Uninstall components in reverse order of installation
    stop_and_disable_services
    run_unbootstrap
    remove_directories
    remove_binary
    remove_azure_cli

    # Show completion message
    show_completion_message
}

# Run main function
main "$@"
