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
    echo ""
    echo -e "${YELLOW}NOTE: This will NOT disconnect the machine from Azure Arc. Use 'aks-flex-node unbootstrap' first if needed.${NC}"
    echo ""

    # Check if running interactively
    if [[ -t 0 ]]; then
        read -p "Are you sure you want to continue? (y/N): " -n 1 -r
        echo
        if [[ ! $REPLY =~ ^[Yy]$ ]]; then
            echo "Uninstall cancelled."
            exit 0
        fi
    else
        log_warning "Running in non-interactive mode. Use --force to skip confirmation."
        if [[ "${1:-}" != "--force" ]]; then
            log_error "Uninstall cancelled. Use --force flag to proceed without confirmation."
            exit 1
        fi
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

check_arc_prerequisites() {
    log_info "Checking Arc prerequisites..."

    # Check if azcmagent is available and machine is connected
    if command -v azcmagent &> /dev/null; then
        # Check if machine is connected to Arc by parsing the actual status
        local arc_status
        arc_status=$(azcmagent show 2>/dev/null | grep "Agent Status" | awk -F: '{print $2}' | xargs)

        if [[ "$arc_status" == "Connected" ]]; then
            log_error "❌ Machine is still connected to Azure Arc!"
            log_error ""
            log_error "You must run 'aks-flex-node unbootstrap' first to:"
            log_error "  • Cleanly remove the node from the AKS cluster"
            log_error "  • Disconnect the machine from Azure Arc"
            log_error "  • Clean up Azure resources properly"
            log_error ""
            log_error "After unbootstrap completes successfully, you can run this uninstall script."
            log_error ""
            log_error "Command to run:"
            log_error "  aks-flex-node unbootstrap"
            exit 1
        else
            log_info "✅ Machine is not connected to Azure Arc (status: ${arc_status:-unknown}) - safe to proceed"
        fi
    else
        log_info "Azure Arc agent not found - safe to proceed"
    fi
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

cleanup_arc_agent() {
    log_info "Checking Azure Arc agent..."

    # Note: We don't uninstall the Arc agent by default as it might be used by other services
    # Users can manually uninstall it if needed
    if command -v azcmagent &> /dev/null; then
        log_warning "Azure Arc agent is still installed"
        log_warning "To remove it manually, run: sudo apt remove azcmagent"
        log_warning "Or follow Microsoft's official uninstall instructions"
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
    echo ""
    echo -e "${YELLOW}What was NOT removed:${NC}"
    echo "ℹ️  Azure Arc agent (azcmagent) - remove manually if not needed"
    echo "ℹ️  Azure Arc connection (disconnection handled by 'aks-flex-node unbootstrap')"
    echo ""
    echo -e "${GREEN}Uninstallation complete!${NC}"
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

    # Check prerequisites before proceeding
    check_arc_prerequisites

    # Uninstall components in reverse order of installation
    stop_and_disable_services
    remove_systemd_service
    remove_sudo_permissions
    remove_service_user
    remove_directories
    remove_binary
    cleanup_arc_agent

    # Show completion message
    show_completion_message
}

# Run main function
main "$@"