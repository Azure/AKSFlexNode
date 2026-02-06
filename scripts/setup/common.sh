#!/bin/bash
# Common functions and environment loading for AKSFlexNode setup scripts

# ============================================================================
# Load Environment Variables
# ============================================================================

load_env() {
    local env_file="${1:-.env}"

    if [ ! -f "$env_file" ]; then
        echo "Error: Environment file '$env_file' not found"
        echo ""
        echo "Please create it from the example:"
        echo "  cp .env.example $env_file"
        echo "  # Edit $env_file with your values"
        return 1
    fi

    # Load .env file, ignoring comments and empty lines
    set -a
    source <(grep -v '^#' "$env_file" | grep -v '^$' | sed 's/\r$//')
    set +a

    return 0
}

# ============================================================================
# Validation Functions
# ============================================================================

validate_required_vars() {
    local missing_vars=()

    # Check each required variable
    for var in "$@"; do
        if [ -z "${!var}" ]; then
            missing_vars+=("$var")
        fi
    done

    if [ ${#missing_vars[@]} -gt 0 ]; then
        echo "Error: Required environment variables are not set:"
        for var in "${missing_vars[@]}"; do
            echo "  - $var"
        done
        echo ""
        echo "Please update your .env file with these values."
        return 1
    fi

    return 0
}

validate_azure_login() {
    if ! az account show &>/dev/null; then
        echo "Error: Not logged in to Azure CLI"
        echo "Please run: az login"
        return 1
    fi

    return 0
}

# ============================================================================
# Color Output Functions
# ============================================================================

# Color codes
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
MAGENTA='\033[0;35m'
NC='\033[0m' # No Color

print_header() {
    echo -e "\n${BLUE}============================================================================${NC}"
    echo -e "${BLUE}$1${NC}"
    echo -e "${BLUE}============================================================================${NC}\n"
}

print_success() {
    echo -e "${GREEN}✅ $1${NC}"
}

print_error() {
    echo -e "${RED}❌ $1${NC}"
}

print_warning() {
    echo -e "${YELLOW}⚠️  $1${NC}"
}

print_info() {
    echo -e "${BLUE}ℹ️  $1${NC}"
}

print_step() {
    echo -e "${CYAN}▶ $1${NC}"
}

# ============================================================================
# Display Functions
# ============================================================================

print_config() {
    print_header "Current Configuration"
    echo "Azure Subscription:  ${AZURE_SUBSCRIPTION_ID:-<not set>}"
    echo "Azure Tenant:        ${AZURE_TENANT_ID:-<not set>}"
    echo "Resource Group:      ${E2E_RESOURCE_GROUP:-<not set>}"
    echo "Location:            ${E2E_LOCATION:-<not set>}"
    echo "Cluster Name:        ${E2E_AKS_CLUSTER_NAME:-<not set>}"
    echo "Node Count:          ${E2E_NODE_COUNT:-1}"
    echo "Node VM Size:        ${E2E_NODE_VM_SIZE:-Standard_B2s}"
    echo "Kubernetes Version:  ${E2E_K8S_VERSION:-Latest Stable}"
    echo ""
}

# ============================================================================
# Confirmation Functions
# ============================================================================

confirm_action() {
    local prompt="${1:-Continue?}"
    local default="${2:-N}"

    if [ "$default" == "Y" ] || [ "$default" == "y" ]; then
        read -p "$prompt (Y/n): " -n 1 -r
        echo
        if [[ $REPLY =~ ^[Nn]$ ]]; then
            return 1
        fi
    else
        read -p "$prompt (y/N): " -n 1 -r
        echo
        if [[ ! $REPLY =~ ^[Yy]$ ]]; then
            return 1
        fi
    fi

    return 0
}

# ============================================================================
# Azure Helper Functions
# ============================================================================

get_subscription_name() {
    az account show --query name -o tsv 2>/dev/null
}

get_cluster_resource_id() {
    local rg="${1:-$E2E_RESOURCE_GROUP}"
    local cluster="${2:-$E2E_AKS_CLUSTER_NAME}"

    az aks show \
        --resource-group "$rg" \
        --name "$cluster" \
        --query id -o tsv 2>/dev/null
}

check_cluster_exists() {
    local rg="${1:-$E2E_RESOURCE_GROUP}"
    local cluster="${2:-$E2E_AKS_CLUSTER_NAME}"

    az aks show \
        --resource-group "$rg" \
        --name "$cluster" \
        &>/dev/null
}

get_cluster_state() {
    local rg="${1:-$E2E_RESOURCE_GROUP}"
    local cluster="${2:-$E2E_AKS_CLUSTER_NAME}"

    az aks show \
        --resource-group "$rg" \
        --name "$cluster" \
        --query powerState.code -o tsv 2>/dev/null
}

# ============================================================================
# .env Update Functions
# ============================================================================

update_env_var() {
    local key="$1"
    local value="$2"
    local env_file="${3:-.env}"

    if [ ! -f "$env_file" ]; then
        echo "Error: $env_file not found"
        return 1
    fi

    # Check if variable exists
    if grep -q "^${key}=" "$env_file"; then
        # Update existing variable
        if [[ "$OSTYPE" == "darwin"* ]]; then
            sed -i '' "s|^${key}=.*|${key}=${value}|" "$env_file"
        else
            sed -i "s|^${key}=.*|${key}=${value}|" "$env_file"
        fi
    else
        # Append new variable
        echo "${key}=${value}" >> "$env_file"
    fi

    print_info "Updated $key in $env_file"
}

# ============================================================================
# Export functions for use in other scripts
# ============================================================================

export -f load_env
export -f validate_required_vars
export -f validate_azure_login
export -f print_header
export -f print_success
export -f print_error
export -f print_warning
export -f print_info
export -f print_step
export -f print_config
export -f confirm_action
export -f get_subscription_name
export -f get_cluster_resource_id
export -f check_cluster_exists
export -f get_cluster_state
export -f update_env_var
