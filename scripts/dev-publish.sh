#!/bin/bash
# AKS Flex Node Dev Publish Script
# This script builds the project, creates a storage account with static website support,
# and uploads the scripts and binary archives for development/testing purposes.

set -euo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Configuration - these can be overridden via command line arguments or environment variables
REPO="Azure/AKSFlexNode"
RESOURCE_GROUP="${DEV_PUBLISH_RESOURCE_GROUP:-}"
STORAGE_ACCOUNT="${DEV_PUBLISH_STORAGE_ACCOUNT:-}"
LOCATION="${DEV_PUBLISH_LOCATION:-eastus}"
VERSION="${DEV_PUBLISH_VERSION:-}"

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

show_help() {
    echo "Usage: $0 [OPTIONS]"
    echo ""
    echo "Build and publish AKS Flex Node binaries to Azure Storage for development/testing."
    echo ""
    echo "Options:"
    echo "  --resource-group, -g NAME    Azure resource group name (required)"
    echo "  --storage-account, -s NAME   Azure storage account name (required)"
    echo "  --location, -l LOCATION      Azure location (default: eastus)"
    echo "  --version, -v VERSION        Version tag for the release (default: git describe)"
    echo "  --help, -h                   Show this help message"
    echo ""
    echo "Environment variables:"
    echo "  DEV_PUBLISH_RESOURCE_GROUP   Resource group name"
    echo "  DEV_PUBLISH_STORAGE_ACCOUNT  Storage account name"
    echo "  DEV_PUBLISH_LOCATION         Azure location"
    echo "  DEV_PUBLISH_VERSION          Version tag"
    echo ""
    echo "Example:"
    echo "  $0 --resource-group my-rg --storage-account myaksflexstorage --version v0.1.0-dev"
    echo ""
    echo "After publishing, use the install script with:"
    echo "  curl -fsSL https://<storage-account>.z13.web.core.windows.net/scripts/install.sh | sudo bash -s -- --download-binary-base-url https://<storage-account>.z13.web.core.windows.net"
}

parse_args() {
    while [[ $# -gt 0 ]]; do
        case $1 in
            --resource-group|-g)
                if [[ -n "${2:-}" ]]; then
                    RESOURCE_GROUP="$2"
                    shift 2
                else
                    log_error "--resource-group requires a value"
                    exit 1
                fi
                ;;
            --storage-account|-s)
                if [[ -n "${2:-}" ]]; then
                    STORAGE_ACCOUNT="$2"
                    shift 2
                else
                    log_error "--storage-account requires a value"
                    exit 1
                fi
                ;;
            --location|-l)
                if [[ -n "${2:-}" ]]; then
                    LOCATION="$2"
                    shift 2
                else
                    log_error "--location requires a value"
                    exit 1
                fi
                ;;
            --version|-v)
                if [[ -n "${2:-}" ]]; then
                    VERSION="$2"
                    shift 2
                else
                    log_error "--version requires a value"
                    exit 1
                fi
                ;;
            --help|-h)
                show_help
                exit 0
                ;;
            *)
                log_warning "Unknown option: $1"
                shift
                ;;
        esac
    done
}

validate_requirements() {
    log_info "Validating requirements..."

    # Check for required tools
    if ! command -v az &> /dev/null; then
        log_error "Azure CLI (az) is not installed. Please install it first."
        exit 1
    fi

    if ! command -v make &> /dev/null; then
        log_error "make is not installed. Please install it first."
        exit 1
    fi

    if ! command -v go &> /dev/null; then
        log_error "Go is not installed. Please install it first."
        exit 1
    fi

    # Check for required parameters
    if [[ -z "$RESOURCE_GROUP" ]]; then
        log_error "Resource group is required. Use --resource-group or set DEV_PUBLISH_RESOURCE_GROUP"
        exit 1
    fi

    if [[ -z "$STORAGE_ACCOUNT" ]]; then
        log_error "Storage account is required. Use --storage-account or set DEV_PUBLISH_STORAGE_ACCOUNT"
        exit 1
    fi

    # Check Azure CLI login status
    if ! az account show &> /dev/null; then
        log_error "Not logged in to Azure CLI. Please run 'az login' first."
        exit 1
    fi

    log_success "All requirements validated"
}

get_version() {
    if [[ -z "$VERSION" ]]; then
        # Get version from git if not specified
        VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo "dev-$(date +%Y%m%d%H%M%S)")
        log_info "Using auto-detected version: $VERSION"
    else
        log_info "Using specified version: $VERSION"
    fi
}

build_project() {
    log_info "Building project..."

    # Change to project root directory
    cd "$(dirname "$0")/.."

    # Run make build to ensure current platform builds
    log_info "Running make build..."
    make build

    # Build and package for all platforms
    log_info "Running make package-all..."
    make package-all

    log_success "Project built successfully"
}

setup_storage_account() {
    log_info "Setting up Azure storage account..."

    # Check if resource group exists, create if not
    if ! az group show --name "$RESOURCE_GROUP" &> /dev/null; then
        log_info "Creating resource group: $RESOURCE_GROUP"
        az group create --name "$RESOURCE_GROUP" --location "$LOCATION"
    else
        log_info "Resource group already exists: $RESOURCE_GROUP"
    fi

    # Check if storage account exists, create if not
    if ! az storage account show --name "$STORAGE_ACCOUNT" --resource-group "$RESOURCE_GROUP" &> /dev/null; then
        log_info "Creating storage account: $STORAGE_ACCOUNT"
        az storage account create \
            --name "$STORAGE_ACCOUNT" \
            --resource-group "$RESOURCE_GROUP" \
            --location "$LOCATION" \
            --sku Standard_LRS \
            --kind StorageV2 \
            --allow-blob-public-access true
    else
        log_info "Storage account already exists: $STORAGE_ACCOUNT"
    fi

    # Enable static website hosting
    log_info "Enabling static website hosting..."
    az storage blob service-properties update \
        --account-name "$STORAGE_ACCOUNT" \
        --static-website \
        --index-document index.html \
        --404-document 404.html

    # Get the static website URL
    STATIC_WEBSITE_URL=$(az storage account show \
        --name "$STORAGE_ACCOUNT" \
        --resource-group "$RESOURCE_GROUP" \
        --query "primaryEndpoints.web" \
        --output tsv)

    log_success "Storage account configured with static website: $STATIC_WEBSITE_URL"
}

upload_scripts() {
    log_info "Uploading scripts..."

    # Change to project root directory
    cd "$(dirname "$0")/.."

    # Upload all scripts to $web container under scripts/ path
    az storage blob upload-batch \
        --account-name "$STORAGE_ACCOUNT" \
        --destination '$web' \
        --source scripts/ \
        --destination-path scripts/ \
        --overwrite

    log_success "Scripts uploaded successfully"
}

upload_binaries() {
    log_info "Uploading binary archives..."

    # Change to project root directory
    cd "$(dirname "$0")/.."

    # Path format: ${REPO}/releases/download/${version}/${archive_name}
    # e.g., Azure/AKSFlexNode/releases/download/v0.1.0/aks-flex-node-linux-amd64.tar.gz

    local release_path="${REPO}/releases/download/${VERSION}"

    # Upload AMD64 archive
    if [[ -f "aks-flex-node-linux-amd64.tar.gz" ]]; then
        log_info "Uploading aks-flex-node-linux-amd64.tar.gz..."
        az storage blob upload \
            --account-name "$STORAGE_ACCOUNT" \
            --container-name '$web' \
            --file "aks-flex-node-linux-amd64.tar.gz" \
            --name "${release_path}/aks-flex-node-linux-amd64.tar.gz" \
            --overwrite
    else
        log_error "aks-flex-node-linux-amd64.tar.gz not found"
        exit 1
    fi

    # Upload ARM64 archive
    if [[ -f "aks-flex-node-linux-arm64.tar.gz" ]]; then
        log_info "Uploading aks-flex-node-linux-arm64.tar.gz..."
        az storage blob upload \
            --account-name "$STORAGE_ACCOUNT" \
            --container-name '$web' \
            --file "aks-flex-node-linux-arm64.tar.gz" \
            --name "${release_path}/aks-flex-node-linux-arm64.tar.gz" \
            --overwrite
    else
        log_error "aks-flex-node-linux-arm64.tar.gz not found"
        exit 1
    fi

    log_success "Binary archives uploaded successfully"
}

show_summary() {
    # Get the static website URL (remove trailing slash if present)
    local base_url="${STATIC_WEBSITE_URL%/}"

    echo ""
    log_success "Dev publish completed successfully!"
    echo ""
    echo -e "${YELLOW}Published Resources:${NC}"
    echo "  Storage Account: $STORAGE_ACCOUNT"
    echo "  Resource Group:  $RESOURCE_GROUP"
    echo "  Version:         $VERSION"
    echo "  Base URL:        $base_url"
    echo ""
    echo -e "${YELLOW}Uploaded Files:${NC}"
    echo "  Scripts:         ${base_url}/scripts/"
    echo "  AMD64 Binary:    ${base_url}/${REPO}/releases/download/${VERSION}/aks-flex-node-linux-amd64.tar.gz"
    echo "  ARM64 Binary:    ${base_url}/${REPO}/releases/download/${VERSION}/aks-flex-node-linux-arm64.tar.gz"
    echo ""
    echo -e "${YELLOW}To install using this dev build:${NC}"
    echo ""
    echo -e "${BLUE}Option 1: Download and run install script${NC}"
    echo "  curl -fsSL ${base_url}/scripts/install.sh | sudo bash -s -- --download-binary-base-url ${base_url}"
    echo ""
    echo -e "${BLUE}Option 2: Download install script first, then run${NC}"
    echo "  curl -fsSL ${base_url}/scripts/install.sh -o install.sh"
    echo "  chmod +x install.sh"
    echo "  sudo ./install.sh --download-binary-base-url ${base_url}"
    echo ""
}

main() {
    echo -e "${GREEN}AKS Flex Node Dev Publish${NC}"
    echo -e "${GREEN}==========================${NC}"
    echo ""

    # Parse command line arguments
    parse_args "$@"

    # Validate requirements
    validate_requirements

    # Get version
    get_version

    # Build the project
    build_project

    # Setup storage account
    setup_storage_account

    # Upload scripts
    upload_scripts

    # Upload binaries
    upload_binaries

    # Show summary
    show_summary
}

# Run main function
main "$@"
