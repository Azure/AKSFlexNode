#!/bin/bash
# AKS Flex Node Installation Script
# This script downloads and installs an AKS Flex Node binary from GitHub releases or a custom archive URL.
#
# Scope: first-time installation only. Once the agent is running, /usr/local/bin/aks-flex-node is a
# symlink into the managed blue/green layout and must be updated through the agent upgrade flow;
# this script refuses to write over that symlink.

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
INSTALL_DIR="/usr/local/bin"
MANAGED_BINARY_DIR="/usr/local/lib/aks-flex-node"
CONFIG_DIR="/etc/aks-flex-node"
DATA_DIR="/var/lib/aks-flex-node"
LOG_DIR="/var/log/aks-flex-node"
GITHUB_API="https://api.github.com/repos/${REPO}"
GITHUB_RELEASES="${GITHUB_API}/releases"
ASSUME_YES=false
# Release tag to install; skips the GitHub latest-release API lookup.
AKS_FLEX_NODE_VERSION="${AKS_FLEX_NODE_VERSION:-}"
# Full release archive URL; bypasses the default GitHub URL layout.
# Set AKS_FLEX_NODE_VERSION with it to avoid the GitHub latest-release API lookup.
AKS_FLEX_NODE_DOWNLOAD_URL="${AKS_FLEX_NODE_DOWNLOAD_URL:-}"
LOCAL_BINARY_PATH="${AKS_FLEX_NODE_LOCAL_BINARY:-}"

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
            log_error "AKS Flex Node supports: Linux"
            exit 1
            ;;
    esac
}

load_os_release() {
    if [[ ! -f /etc/os-release ]]; then
        return 1
    fi

    ID=""
    VERSION_ID=""
    PRETTY_NAME=""

    while IFS='=' read -r key value; do
        value="${value%\"}"
        value="${value#\"}"

        case "$key" in
            ID)
                ID="$value"
                ;;
            VERSION_ID)
                VERSION_ID="$value"
                ;;
            PRETTY_NAME)
                PRETTY_NAME="$value"
                ;;
        esac
    done < /etc/os-release
}

check_linux_distribution() {
    if load_os_release; then
        case "$ID" in
            ubuntu)
                case "$VERSION_ID" in
                    "22.04"|"24.04")
                        log_info "Detected Ubuntu $VERSION_ID LTS - supported"
                        return 0
                        ;;
                    *)
                        log_warning "Detected Ubuntu $VERSION_ID - not officially supported"
                        ;;
                esac
                ;;
            azurelinux|azlinux)
                case "${VERSION_ID%%.*}" in
                    3)
                        log_info "Detected Azure Linux $VERSION_ID - supported"
                        return 0
                        ;;
                    *)
                        log_warning "Detected Azure Linux $VERSION_ID - not officially supported"
                        ;;
                esac
                ;;
            almalinux|rocky|rhel)
                case "${VERSION_ID%%.*}" in
                    9|10)
                        log_info "Detected $PRETTY_NAME - supported"
                        return 0
                        ;;
                    *)
                        log_warning "Detected $PRETTY_NAME - not officially supported"
                        ;;
                esac
                ;;
            *)
                if command -v dnf &> /dev/null; then
                    log_warning "Detected $PRETTY_NAME - not officially supported, but dnf is available"
                else
                    log_warning "Detected $PRETTY_NAME - not officially supported"
                fi
                ;;
        esac

        log_warning "AKS Flex Node is tested on Ubuntu 22.04/24.04 LTS, Azure Linux 3.x, and RHEL-family 9/10"
        log_warning "Continuing installation but support may be limited"
    else
        log_warning "Cannot detect OS version - continuing installation"
    fi
}

get_latest_release() {
    if [[ -n "$AKS_FLEX_NODE_VERSION" ]]; then
        echo "$AKS_FLEX_NODE_VERSION"
        return 0
    fi

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

    if [[ -n "$LOCAL_BINARY_PATH" ]]; then
        log_info "Using local AKS Flex Node binary override: $LOCAL_BINARY_PATH" >&2
        if [[ ! -f "$LOCAL_BINARY_PATH" ]]; then
            log_error "Local binary override not found: $LOCAL_BINARY_PATH"
            exit 1
        fi
        echo "$LOCAL_BINARY_PATH"
        return 0
    fi

    local binary_name="aks-flex-node-${os}-${arch}"
    local archive_name="${binary_name}.tar.gz"
    local download_url="${AKS_FLEX_NODE_DOWNLOAD_URL:-https://github.com/${REPO}/releases/download/${version}/${archive_name}}"

    log_info "Downloading AKS Flex Node ${version} for ${os}/${arch}..." >&2
    if [[ -n "$AKS_FLEX_NODE_DOWNLOAD_URL" ]]; then
        # Custom URLs may contain embedded credentials or signed query parameters.
        log_info "Using custom AKS Flex Node download URL" >&2
    else
        log_info "Download URL: $download_url" >&2
    fi

    local temp_dir
    temp_dir=$(mktemp -d)
    cd "$temp_dir"

    if command -v curl &> /dev/null; then
        if ! curl -fsSL -o "$archive_name" "$download_url"; then
            log_error "Failed to download $archive_name"
            rm -rf "$temp_dir"
            exit 1
        fi
    elif command -v wget &> /dev/null; then
        if ! wget -qO "$archive_name" "$download_url"; then
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
    local install_args=(-m 0755)
    local target_path="$INSTALL_DIR/aks-flex-node"

    log_info "Installing binary to $INSTALL_DIR..."

    if [[ $EUID -eq 0 ]]; then
        install_args=(-o root -g root -m 0755)
    else
        log_warning "Installing binary as $(id -un); run the installer with sudo to make it root-owned"
    fi

    if [[ -L "$target_path" ]]; then
        # Once the agent has started, this path is a symlink into the managed blue/green layout
        # under $MANAGED_BINARY_DIR. Updating that layout requires the agent upgrade protocol,
        # which serializes activation with the host upgrade lock, preserves the last-good slot,
        # and synchronizes the nspawn binary. Writing here would race with it, so this installer
        # only performs first-time installation.
        log_error "Refusing to replace the symbolic link at $target_path; this installer only performs first-time installation."
        log_error "Update an already-installed agent through the agent upgrade flow, or remove $target_path (and the managed layout in $MANAGED_BINARY_DIR) to reinstall from scratch."
        return 1
    fi

    if [[ -d "$target_path" ]]; then
        # "mv" would move the staged file into the directory instead of replacing the binary.
        log_error "Refusing to install binary; $target_path is a directory"
        return 1
    fi

    (
        staged=""
        if ! staged=$(mktemp "$INSTALL_DIR/.aks-flex-node.XXXXXX"); then
            log_error "Failed to create staged binary in $INSTALL_DIR"
            exit 1
        fi
        trap '[[ -z "${staged:-}" ]] || rm -f "$staged"' EXIT
        if ! install "${install_args[@]}" "$binary_path" "$staged"; then
            log_error "Failed to stage binary at $staged"
            exit 1
        fi
        if ! mv -f "$staged" "$target_path"; then
            log_error "Failed to install binary to $target_path"
            exit 1
        fi
        staged=""
    ) || return 1

    log_success "Binary installed to $INSTALL_DIR/aks-flex-node"
}

warn_install_dir_not_in_path() {
    case ":${PATH:-}:" in
        *:"$INSTALL_DIR":*) ;;
        *) log_warning "$INSTALL_DIR is not in PATH; add it to PATH to run aks-flex-node without the full path." ;;
    esac
}

setup_directories() {
    log_info "Creating directories..."

    # Create directories
    mkdir -p "$CONFIG_DIR" "$DATA_DIR" "$LOG_DIR"
    chown root:root "$CONFIG_DIR"
    chown root:root "$DATA_DIR" "$LOG_DIR"
    chmod 755 "$CONFIG_DIR" "$DATA_DIR" "$LOG_DIR"

    # Ensure log file can be created with correct permissions
    touch "$LOG_DIR/aks-flex-node.log"
    chown root:root "$LOG_DIR/aks-flex-node.log"
    chmod 644 "$LOG_DIR/aks-flex-node.log"

    log_success "Directories created successfully"
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
    "resourceManagerEndpoint": "https://management.azure.com",
    "targetAgentPoolName": "YOUR_AGENT_POOL_NAME",
    "arc": { "enabled": true },
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
    echo "  Bootstrap node:         aks-flex-node bootstrap --config $CONFIG_DIR/config.json"
    echo "  Run daemon directly:    aks-flex-node daemon --config $CONFIG_DIR/config.json"
    echo "  Reset node:             aks-flex-node reset"
    echo "  Check version:          aks-flex-node version"
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
    # Check for force/yes flag
    if [[ "${1:-}" == "--yes" || "${1:-}" == "-y" ]]; then
        ASSUME_YES=true
    fi

    echo -e "${GREEN}AKS Flex Node Installer${NC}"
    echo -e "${GREEN}========================${NC}"
    echo ""

    # Check if running as root
    if [[ $EUID -ne 0 ]]; then
        log_error "This script must be run as root (use sudo)"
        exit 1
    fi

    # Check OS compatibility
    check_linux_distribution

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
    warn_install_dir_not_in_path

    # Azure authentication is provided by the selected runtime identity. The
    # installer does not install Azure CLI or rely on a user's cached login.
    setup_directories

    # Cleanup only the temp download directory created by this installer.
    # Local binary overrides are caller-owned and may share a directory with
    # other e2e artifacts such as /tmp/config.json.
    if [[ -z "$LOCAL_BINARY_PATH" ]]; then
        rm -rf "$(dirname "$binary_path")"
    fi

    # Show next steps
    show_next_steps
}

if [[ "${BASH_SOURCE[0]:-$0}" == "$0" ]]; then
    main "$@"
fi
