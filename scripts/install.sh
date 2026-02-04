#!/bin/bash
# AKS Flex Node Installation Script
# This script downloads and installs the latest AKS Flex Node binary from GitHub releases

set -euo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Configuration
REPO="Azure/AKSFlexNode"
SERVICE_NAME="aks-flex-node"
SERVICE_USER="aks-flex-node"
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/aks-flex-node"
DATA_DIR="/var/lib/aks-flex-node"
LOG_DIR="/var/log/aks-flex-node"
GITHUB_API="https://api.github.com/repos/${REPO}"
GITHUB_RELEASES="${GITHUB_API}/releases"

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

detect_architecture() {
    local arch
    arch=$(uname -m)
    case $arch in
        x86_64)
            echo "amd64"
            ;;
        aarch64)
            echo "arm64"
            ;;
        *)
            log_error "Unsupported architecture: $arch"
            log_error "AKS Flex Node supports: x86_64 (amd64), aarch64 (arm64)"
            exit 1
            ;;
    esac
}

detect_os() {
    local os
    os=$(uname -s | tr '[:upper:]' '[:lower:]')
    case $os in
        linux)
            echo "linux"
            ;;
        *)
            log_error "Unsupported operating system: $os"
            log_error "AKS Flex Node supports: Linux (Ubuntu 22.04 LTS, Ubuntu 24.04 LTS)"
            exit 1
            ;;
    esac
}

check_ubuntu_version() {
    if [[ -f /etc/os-release ]]; then
        source /etc/os-release
        if [[ "$ID" == "ubuntu" ]]; then
            case "$VERSION_ID" in
                "22.04"|"24.04")
                    log_info "Detected Ubuntu $VERSION_ID LTS - supported"
                    return 0
                    ;;
                *)
                    log_warning "Detected Ubuntu $VERSION_ID - not officially supported"
                    log_warning "AKS Flex Node is tested on Ubuntu 22.04 LTS and Ubuntu 24.04 LTS"
                    log_warning "Continuing installation but support may be limited"
                    ;;
            esac
        else
            log_warning "Detected $PRETTY_NAME - not officially supported"
            log_warning "AKS Flex Node is tested on Ubuntu 22.04 LTS and Ubuntu 24.04 LTS"
            log_warning "Continuing installation but support may be limited"
        fi
    else
        log_warning "Cannot detect OS version - continuing installation"
    fi
}

get_latest_release() {
    local latest_release_url="${GITHUB_RELEASES}/latest"
    log_info "Fetching latest release information..." >&2

    if command -v curl &> /dev/null; then
        curl -s "$latest_release_url" | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/'
    elif command -v wget &> /dev/null; then
        wget -qO- "$latest_release_url" | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/'
    else
        log_error "Neither curl nor wget is available. Please install one of them."
        exit 1
    fi
}

download_binary() {
    local version="$1"
    local os="$2"
    local arch="$3"
    local binary_name="aks-flex-node-${os}-${arch}"
    local archive_name="${binary_name}.tar.gz"
    local download_url="https://github.com/${REPO}/releases/download/${version}/${archive_name}"

    log_info "Downloading AKS Flex Node ${version} for ${os}/${arch}..." >&2
    log_info "Download URL: $download_url" >&2

    local temp_dir
    temp_dir=$(mktemp -d)
    cd "$temp_dir"

    if command -v curl &> /dev/null; then
        if ! curl -L -f -o "$archive_name" "$download_url"; then
            log_error "Failed to download $archive_name"
            rm -rf "$temp_dir"
            exit 1
        fi
    elif command -v wget &> /dev/null; then
        if ! wget -O "$archive_name" "$download_url"; then
            log_error "Failed to download $archive_name"
            rm -rf "$temp_dir"
            exit 1
        fi
    else
        log_error "Neither curl nor wget is available. Please install one of them."
        exit 1
    fi

    log_info "Extracting binary..." >&2
    tar -xzf "$archive_name"

    if [[ ! -f "$binary_name" ]]; then
        log_error "Binary not found in archive"
        rm -rf "$temp_dir"
        exit 1
    fi

    echo "$temp_dir/$binary_name"
}

install_binary() {
    local binary_path="$1"

    log_info "Installing binary to $INSTALL_DIR..."

    # Install binary
    cp "$binary_path" "$INSTALL_DIR/aks-flex-node"
    chmod +x "$INSTALL_DIR/aks-flex-node"
    chown root:root "$INSTALL_DIR/aks-flex-node"

    log_success "Binary installed to $INSTALL_DIR/aks-flex-node"
}

setup_service_user() {
    log_info "Setting up service user..."

    # Create service user
    if ! id "$SERVICE_USER" &>/dev/null; then
        useradd --system --shell /bin/false --home-dir "$DATA_DIR" --create-home "$SERVICE_USER"
        log_success "Created service user: $SERVICE_USER"
    else
        log_info "Service user $SERVICE_USER already exists"
    fi
}

install_azure_cli() {
    log_info "Installing Azure CLI..."

    if ! command -v az &> /dev/null; then
        log_info "Downloading and installing Azure CLI..."
        if command -v curl &> /dev/null; then
            curl -sL https://aka.ms/InstallAzureCLIDeb | bash </dev/null
        elif command -v wget &> /dev/null; then
            wget -qO- https://aka.ms/InstallAzureCLIDeb | bash </dev/null
        else
            log_error "Neither curl nor wget is available for downloading Azure CLI"
            return 1
        fi

        # Verify installation
        if command -v az &> /dev/null; then
            log_success "Azure CLI installed successfully"
        else
            log_error "Azure CLI installation failed"
            return 1
        fi
    else
        log_info "Azure CLI already installed"
    fi
}

check_azure_cli_auth() {
    log_info "Checking Azure CLI authentication..."

    # Check if the user who ran sudo is authenticated
    local current_user="${SUDO_USER:-}"
    if [[ -n "$current_user" ]]; then
        log_info "Checking Azure CLI authentication for user: $current_user"

        if sudo -u "$current_user" az account show &>/dev/null; then
            log_success "Azure CLI is authenticated for user: $current_user"

            # Check if .azure directory exists
            local azure_dir
            azure_dir=$(eval echo "~$current_user")/.azure

            if [[ -d "$azure_dir" ]]; then
                log_info "Azure CLI configuration found at: $azure_dir"
                log_info "Will configure permissions for service access"
            else
                log_warning "Azure CLI directory not found at: $azure_dir"
            fi
        else
            log_warning "Azure CLI is not authenticated for user: $current_user"
            log_info ""
            log_info "If you want to use Azure CLI authentication:"
            log_info "  1. Exit this installer (Ctrl+C)"
            log_info "  2. Run 'az login' as user $current_user"
            log_info "  3. Then re-run this installer with sudo"
            log_info ""
            echo -n "Do you want to continue anyway? (service principal or managed identity auth only) [y/N]: "
            read -r response </dev/tty
            case "$response" in
                [yY]|[yY][eE][sS])
                    log_info "Continuing without CLI authentication. Make sure to configure service principal or managed identity."
                    return 0
                    ;;
                *)
                    log_info "Installation cancelled. Please run 'az login' first."
                    exit 0
                    ;;
            esac
        fi
    else
        log_warning "Could not detect original user (SUDO_USER not set)"
        log_info "Make sure to run 'az login' before configuring the service"
    fi
}

setup_permissions() {
    log_info "Setting up permissions..."

    # Add service user to himds group (created by Arc agent installation)
    usermod -a -G himds "$SERVICE_USER"

    # Configure Azure CLI access for service user
    local current_user
    current_user=$(logname 2>/dev/null || echo "${SUDO_USER:-$USER}")
    local current_user_home
    current_user_home=$(eval echo "~$current_user")

    if [[ -d "$current_user_home/.azure" ]]; then
        # Add service user to current user's group for Azure CLI access
        usermod -a -G "$current_user" "$SERVICE_USER"

        # Set group ownership and permissions on Azure CLI directory
        chgrp -R "$current_user" "$current_user_home/.azure"

        # Set group read/write/execute permissions on directory and subdirectories
        # Use setgid bit (g+s) so new files inherit the group ownership
        find "$current_user_home/.azure" -type d -exec chmod g+rwxs {} \;

        # Set group read/write permissions on all existing files
        find "$current_user_home/.azure" -type f -exec chmod g+rw {} \; 

    log_success "Azure CLI access configured for service user (user: $current_user)"
    else
        log_warning "Azure CLI not found at $current_user_home/.azure - skipping CLI access setup"
    fi
}

setup_hostname_resolution() {
    log_info "Setting up hostname resolution..."

    local current_hostname
    current_hostname=$(hostname)

    # Check if hostname is already in /etc/hosts
    if grep -q "$current_hostname" /etc/hosts; then
        log_info "Hostname $current_hostname already configured in /etc/hosts"
    else
        log_info "Adding hostname $current_hostname to /etc/hosts"
        echo "127.0.1.1 $current_hostname" >> /etc/hosts
        log_success "Hostname resolution configured for $current_hostname"
    fi
}

setup_directories() {
    log_info "Creating directories..."

    # Create directories
    mkdir -p "$CONFIG_DIR" "$DATA_DIR" "$LOG_DIR"
    chown root:root "$CONFIG_DIR"
    chown "$SERVICE_USER:$SERVICE_USER" "$DATA_DIR" "$LOG_DIR"
    chmod 755 "$CONFIG_DIR" "$DATA_DIR" "$LOG_DIR"

    # Ensure log file can be created with correct permissions
    touch "$LOG_DIR/aks-flex-node.log"
    chown "$SERVICE_USER:$SERVICE_USER" "$LOG_DIR/aks-flex-node.log"
    chmod 644 "$LOG_DIR/aks-flex-node.log"

    log_success "Directories created successfully"
}

setup_sudo_permissions() {
    log_info "Setting up sudo permissions for service user..."

    # Download sudoers file from repository
    local temp_dir
    temp_dir=$(mktemp -d)
    local sudoers_url="https://raw.githubusercontent.com/${REPO}/${version}/aks-flex-node-sudoers"

    if command -v curl &> /dev/null; then
        if ! curl -L -f -o "$temp_dir/aks-flex-node-sudoers" "$sudoers_url"; then
            log_error "Failed to download sudoers configuration"
            rm -rf "$temp_dir"
            return 1
        fi
    elif command -v wget &> /dev/null; then
        if ! wget -O "$temp_dir/aks-flex-node-sudoers" "$sudoers_url"; then
            log_error "Failed to download sudoers configuration"
            rm -rf "$temp_dir"
            return 1
        fi
    else
        log_error "Neither curl nor wget is available for downloading sudoers configuration"
        return 1
    fi

    # Install sudoers file
    cp "$temp_dir/aks-flex-node-sudoers" /etc/sudoers.d/aks-flex-node
    chmod 440 /etc/sudoers.d/aks-flex-node
    chown root:root /etc/sudoers.d/aks-flex-node

    # Validate sudoers syntax
    if ! visudo -c -f /etc/sudoers.d/aks-flex-node; then
        log_error "Invalid sudoers configuration. Removing..."
        rm -f /etc/sudoers.d/aks-flex-node
        rm -rf "$temp_dir"
        return 1
    fi

    rm -rf "$temp_dir"
    log_success "Sudo permissions configured successfully"
    return 0
}

setup_systemd_service() {
    log_info "Setting up systemd service..."

    # Download service file from repository (use the same version as the binary)
    local temp_dir
    temp_dir=$(mktemp -d)
    local service_url="https://raw.githubusercontent.com/${REPO}/${version}/aks-flex-node-agent.service"

    if command -v curl &> /dev/null; then
        if ! curl -L -f -o "$temp_dir/aks-flex-node-agent.service" "$service_url"; then
            log_error "Failed to download systemd service file"
            rm -rf "$temp_dir"
            return 1
        fi
    elif command -v wget &> /dev/null; then
        if ! wget -O "$temp_dir/aks-flex-node-agent.service" "$service_url"; then
            log_error "Failed to download systemd service file"
            rm -rf "$temp_dir"
            return 1
        fi
    else
        log_error "Neither curl nor wget is available for downloading service file"
        return 1
    fi

    # Install systemd service file
    cp "$temp_dir/aks-flex-node-agent.service" /etc/systemd/system/
    chmod 644 /etc/systemd/system/aks-flex-node-agent.service

    # Update the service file with the correct user path for Azure CLI access
    local current_user
    current_user=$(logname 2>/dev/null || echo "${SUDO_USER:-$USER}")
    local current_user_home
    current_user_home=$(eval echo "~$current_user")

    log_info "Configuring service file for current user ($current_user)..."
    sed -i "s|PLACEHOLDER_AZURE_CONFIG_DIR|$current_user_home/.azure|g" /etc/systemd/system/aks-flex-node-agent.service
    sed -i "s|PLACEHOLDER_USER_GROUP|$current_user|g" /etc/systemd/system/aks-flex-node-agent.service

    # Reload systemd
    systemctl daemon-reload

    rm -rf "$temp_dir"
    log_success "Systemd service configured successfully"
    return 0
}

show_next_steps() {
    log_success "AKS Flex Node installation completed successfully!"
    echo ""
    echo -e "${YELLOW}Next Steps:${NC}"
    echo "1. Create configuration file: $CONFIG_DIR/config.json"
    echo ""
    echo -e "${YELLOW}Example configuration:${NC}"
    cat << 'EOF'
{
  "azure": {
    "subscriptionId": "YOUR_SUBSCRIPTION_ID",
    "tenantId": "YOUR_TENANT_ID",
    "cloud": "AzurePublicCloud",
    "arc": {
      "machineName": "YOUR_MACHINE_NAME",
      "tags": {
        "node-type": "edge"
      },
      "resourceGroup": "YOUR_RESOURCE_GROUP",
      "location": "YOUR_LOCATION"
    },
    "targetCluster": {
      "resourceId": "/subscriptions/YOUR_SUBSCRIPTION_ID/resourceGroups/YOUR_RESOURCE_GROUP/providers/Microsoft.ContainerService/managedClusters/YOUR_CLUSTER_NAME",
      "location": "YOUR_LOCATION"
    }
  },
  "agent": {
    "logLevel": "info",
    "logDir": "/var/log/aks-flex-node"
  }
}
EOF
    echo ""
    echo -e "${YELLOW}Usage Options:${NC}"
    echo ""
    echo -e "${BLUE}Command Line Usage:${NC}"
    echo "  Run agent daemon:       aks-flex-node agent --config $CONFIG_DIR/config.json"
    echo "  Bootstrap node:         aks-flex-node bootstrap --config $CONFIG_DIR/config.json"
    echo "  Unbootstrap node:       aks-flex-node unbootstrap --config $CONFIG_DIR/config.json"
    echo "  Check version:          aks-flex-node version"
    echo ""

    if [[ "${SERVICE_SETUP_SUCCESS:-false}" == "true" ]]; then
        echo -e "${BLUE}Systemd Service Usage:${NC}"
        echo "  Enable agent service:       systemctl enable aks-flex-node-agent.service"
        echo "  Start agent:                systemctl start aks-flex-node-agent"
        echo "  Stop agent:                 systemctl stop aks-flex-node-agent"
        echo "  Check service status:       systemctl status aks-flex-node-agent"
        echo "  View service logs:          journalctl -u aks-flex-node-agent -f"
        echo ""
        echo -e "${GREEN}✅ Systemd service is ready to use!${NC}"
    else
        echo -e "${BLUE}Systemd Service:${NC}"
        echo -e "${YELLOW}⚠️  Systemd service setup was not completed during installation.${NC}"
        echo "  You can still run the service manually using the CLI commands above."
    fi

    echo ""
    echo -e "${YELLOW}Directories:${NC}"
    echo "  Configuration: $CONFIG_DIR"
    echo "  Data:          $DATA_DIR"
    echo "  Logs:          $LOG_DIR"
    echo "  Binary:        $INSTALL_DIR/aks-flex-node"
    echo ""
    echo -e "${YELLOW}Uninstall:${NC}"
    echo "  To uninstall:  curl -fsSL https://raw.githubusercontent.com/${REPO}/${version}/scripts/uninstall.sh | sudo bash -s -- --force"
}

main() {
    echo -e "${GREEN}AKS Flex Node Installer${NC}"
    echo -e "${GREEN}========================${NC}"
    echo ""

    # Check if running as root
    if [[ $EUID -ne 0 ]]; then
        log_error "This script must be run as root (use sudo)"
        exit 1
    fi

    # Check OS compatibility
    check_ubuntu_version

    # Detect system architecture
    local os arch
    os=$(detect_os)
    arch=$(detect_architecture)

    log_info "Detected platform: ${os}/${arch}"

    # Get latest release
    local version
    version=$(get_latest_release)

    if [[ -z "$version" ]]; then
        log_error "Failed to get latest release information"
        exit 1
    fi

    log_info "Latest version: $version"

    # Download binary
    local binary_path
    binary_path=$(download_binary "$version" "$os" "$arch")

    # Install binary
    install_binary "$binary_path"

    # Setup service components
    setup_service_user
    install_azure_cli
    check_azure_cli_auth
    setup_permissions
    setup_hostname_resolution
    setup_directories

    # Setup systemd service components
    if setup_sudo_permissions && setup_systemd_service; then
        log_success "Systemd service setup completed successfully"
        SERVICE_SETUP_SUCCESS=true
    else
        log_warning "Systemd service setup failed - you can still use the CLI directly"
        SERVICE_SETUP_SUCCESS=false
    fi

    # Cleanup
    rm -rf "$(dirname "$binary_path")"

    # Show next steps
    show_next_steps
}

# Run main function
main "$@"