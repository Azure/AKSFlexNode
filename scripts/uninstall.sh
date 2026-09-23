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
DEFAULT_HOST_PREFIX="/usr/local"
INSTALL_DIR="$DEFAULT_HOST_PREFIX/bin"
MANAGED_BINARY_DIR="$DEFAULT_HOST_PREFIX/lib/aks-flex-node"
AKS_FLEX_NODE_HOST_PREFIX="${AKS_FLEX_NODE_HOST_PREFIX:-}"
HOST_PREFIX=""
CONFIG_DIR="/etc/aks-flex-node"
DATA_DIR="/var/lib/aks-flex-node"
LOG_DIR="/var/log/aks-flex-node"
SERVICE_UNIT="aks-flex-node-agent.service"
SERVICE_UNIT_PATH="/etc/systemd/system/$SERVICE_UNIT"
RECOVERY_UNIT_PATH="/etc/systemd/system/aks-flex-node-agent-recovery.service"

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

# resolve_host_prefix finds where the binaries were installed. It must run before reset, which
# removes the config that records agent.hostPrefix.
resolve_host_prefix() {
    local config_path="$CONFIG_DIR/config.json" from_config=""

    if [[ -f "$config_path" ]] && command -v jq &> /dev/null; then
        from_config=$(jq -r '.agent.hostPrefix // empty' "$config_path")
    fi

    HOST_PREFIX="${from_config:-${AKS_FLEX_NODE_HOST_PREFIX:-$DEFAULT_HOST_PREFIX}}"
    [[ "$HOST_PREFIX" == "/" ]] || HOST_PREFIX="${HOST_PREFIX%/}"
    INSTALL_DIR="$HOST_PREFIX/bin"
    MANAGED_BINARY_DIR="$HOST_PREFIX/lib/aks-flex-node"
}

confirm_uninstall() {
    echo -e "${YELLOW}AKS Flex Node Uninstaller${NC}"
    echo -e "${YELLOW}===========================${NC}"
    echo ""
    echo "This will remove the following components:"
    echo "• AKS Flex Node binaries ($INSTALL_DIR/aks-flex-node, $MANAGED_BINARY_DIR)"
    echo "• Systemd service (aks-flex-node-agent.service)"
    echo "• Configuration directory ($CONFIG_DIR)"
    echo "• Data directory ($DATA_DIR)"
    echo "• Log directory ($LOG_DIR)"
    echo "• Host network state created by unbounded-net"
    echo ""
    echo -e "${YELLOW}NOTE: This will first run 'aks-flex-node reset' to clean up local node and host network resources.${NC}"
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

run_reset() {
    log_info "Running reset to clean up local node and host network resources..."

    # Check if aks-flex-node binary exists
    if [[ ! -f "$INSTALL_DIR/aks-flex-node" ]]; then
        log_warning "AKS Flex Node binary not found at $INSTALL_DIR/aks-flex-node"
        log_info "Skipping reset - binary may already be removed"
        log_info "Removing systemd service directly..."

        systemctl stop "$SERVICE_UNIT" 2>/dev/null || true
        systemctl disable "$SERVICE_UNIT" 2>/dev/null || true

        for unit_path in "$SERVICE_UNIT_PATH" "$RECOVERY_UNIT_PATH"; do
            if [[ -e "$unit_path" ]]; then
                rm -f "$unit_path"
                log_success "Removed systemd unit: $unit_path"
            else
                log_info "Systemd unit not found: $unit_path"
            fi
        done

        systemctl daemon-reload 2>/dev/null || true
        return 0
    fi

    env TERM="${TERM:-dumb}" "$INSTALL_DIR/aks-flex-node" reset 2>&1 || {
        log_warning "Reset failed - this may be expected if resources are already cleaned up"
    }

    log_success "Reset completed"
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
    log_info "Removing binaries..."

    # -L as well as -e: once the agent has run this is a symlink into the managed layout, and a
    # dangling one still has to go.
    if [[ -e "$INSTALL_DIR/aks-flex-node" || -L "$INSTALL_DIR/aks-flex-node" ]]; then
        rm -f "$INSTALL_DIR/aks-flex-node"
        log_success "Removed binary: $INSTALL_DIR/aks-flex-node"
    else
        log_info "Binary not found: $INSTALL_DIR/aks-flex-node"
    fi

    # Reset keeps the managed blue/green layout so a reinstall can reuse it; uninstall removes it.
    if [[ -d "$MANAGED_BINARY_DIR" ]]; then
        rm -rf "$MANAGED_BINARY_DIR"
        log_success "Removed managed binaries: $MANAGED_BINARY_DIR"
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
    echo "✅ Host network state"
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

    resolve_host_prefix

    # Confirm uninstall
    confirm_uninstall "${1:-}"

    echo ""
    log_info "Starting AKS Flex Node uninstallation..."

    # Uninstall components in reverse order of installation
    run_reset
    remove_directories
    remove_binary

    # Show completion message
    show_completion_message
}

if [[ "${BASH_SOURCE[0]:-$0}" == "$0" ]]; then
    main "$@"
fi
