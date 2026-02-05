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
SERVICE_NAME="aks-flex-node"
SERVICE_USER="aks-flex-node"
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/aks-flex-node"
DATA_DIR="/var/lib/aks-flex-node"
LOG_DIR="/var/log/aks-flex-node"

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
    echo "• Service user ($SERVICE_USER)"
    echo "• Configuration directory ($CONFIG_DIR)"
    echo "• Data directory ($DATA_DIR)"
    echo "• Log directory ($LOG_DIR)"
    echo "• Sudo permissions (/etc/sudoers.d/aks-flex-node)"
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

    # Stop the agent service if running
    if systemctl is-active --quiet "aks-flex-node-agent"; then
        log_info "Stopping aks-flex-node-agent..."
        systemctl stop "aks-flex-node-agent" || true
    fi

    if systemctl is-enabled --quiet "aks-flex-node-agent" 2>/dev/null; then
        log_info "Disabling aks-flex-node-agent..."
        systemctl disable "aks-flex-node-agent" || true
    fi

    log_success "Services stopped and disabled"
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
        log_warning "Config file not found at $CONFIG_DIR/config.json"
        log_warning "Cannot run unbootstrap without config file - skipping resource cleanup"
        log_info "Manual cleanup of Azure resources may be required"
        return 0
    fi

    # Run unbootstrap to clean up resources
    # Get the current user who ran sudo (the one with Azure CLI credentials)
    local current_user
    current_user=$(logname 2>/dev/null || echo "${SUDO_USER:-$USER}")
    local current_user_home
    current_user_home=$(eval echo "~$current_user")

    # Set Azure CLI environment variable to point to the user's .azure directory
    local azure_config_dir="$current_user_home/.azure"

    if [[ -d "$azure_config_dir" ]]; then
        log_info "Using Azure CLI credentials from: $azure_config_dir"
        
        # Added TERM=$TERM to ensure the tool knows it can print formatted text
        # Added 2>&1 to capture Standard Error logs alongside Standard Out
        sudo env AZURE_CONFIG_DIR="$azure_config_dir" TERM="$TERM" "$INSTALL_DIR/aks-flex-node" unbootstrap --config "$config_file" 2>&1 || {
            log_warning "Unbootstrap failed - this may be expected if resources are already cleaned up"
        }
    else
        log_warning "Azure CLI credentials not found at $azure_config_dir"
        log_info "Attempting unbootstrap without Azure CLI credentials..."
        
        # Applied the same fixes here
        sudo env TERM="$TERM" "$INSTALL_DIR/aks-flex-node" unbootstrap --config "$config_file" 2>&1 || {
            log_warning "Unbootstrap failed - this may be expected if resources are already cleaned up"
        }
    fi

    log_success "Unbootstrap completed"
}

remove_systemd_service() {
    log_info "Removing systemd service files..."

    # Remove agent service file
    if [[ -f "/etc/systemd/system/aks-flex-node-agent.service" ]]; then
        rm -f "/etc/systemd/system/aks-flex-node-agent.service"
        log_success "Removed systemd agent service file"
    else
        log_info "Agent service file not found"
    fi

    # Reload systemd daemon
    systemctl daemon-reload
    log_success "Systemd daemon reloaded"
}

remove_sudo_permissions() {
    log_info "Removing sudo permissions..."

    if [[ -f "/etc/sudoers.d/aks-flex-node" ]]; then
        rm -f "/etc/sudoers.d/aks-flex-node"
        log_success "Removed sudo permissions file"
    else
        log_info "Sudo permissions file not found"
    fi
}

remove_service_user() {
    log_info "Removing service user..."

    if id "$SERVICE_USER" &>/dev/null; then
        # Stop any processes running as the service user
        pkill -u "$SERVICE_USER" || true
        sleep 2

        # Remove the user and their home directory
        userdel -r "$SERVICE_USER" 2>/dev/null || {
            log_warning "Failed to remove user with home directory, trying without -r flag"
            userdel "$SERVICE_USER" 2>/dev/null || log_warning "Failed to remove service user"
        }
        log_success "Removed service user: $SERVICE_USER"
    else
        log_info "Service user $SERVICE_USER not found"
    fi
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
    echo "✅ Sudo permissions"
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

    # Run unbootstrap before proceeding with uninstall
    run_unbootstrap

    # Uninstall components in reverse order of installation
    stop_and_disable_services
    remove_systemd_service
    remove_sudo_permissions
    remove_service_user
    remove_directories
    remove_binary
    remove_azure_cli

    # Show completion message
    show_completion_message
}

# Run main function
main "$@"