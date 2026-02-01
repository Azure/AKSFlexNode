#!/bin/bash
set -euo pipefail

# ============================================================================
# Self-Hosted Runner Setup - Simplified E2E Pipeline
# ============================================================================
# This script creates everything needed for E2E testing:
# 1. Creates runner VM with Managed Identity
# 2. Assigns MSI the proper permissions
# 3. Installs all prerequisites
#
# After this, GitHub workflows can run WITHOUT any Azure credentials!
# ============================================================================

# Get script directory
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

# Load common functions
source "$SCRIPT_DIR/common.sh"

# Load environment
cd "$PROJECT_ROOT"
if ! load_env ".env"; then
    exit 1
fi

# ============================================================================
# Configuration
# ============================================================================

RUNNER_RESOURCE_GROUP="${RUNNER_RESOURCE_GROUP:-rg-aksflexnode-e2e-runner}"
RUNNER_LOCATION="${RUNNER_LOCATION:-westus2}"
RUNNER_VM_NAME="${RUNNER_VM_NAME:-vm-e2e-runner}"
RUNNER_VM_SIZE="${RUNNER_VM_SIZE:-Standard_B2ms}"
RUNNER_USER="azureuser"

print_header "Self-Hosted Runner Setup for E2E Testing"

echo "This script will:"
echo "  1. Create resource group for runner (if needed)"
echo "  2. Create Azure VM with Managed Identity"
echo "  3. Assign MSI permissions (Contributor, Arc, AKS access)"
echo "  4. Install prerequisites (Azure CLI, kubectl, git, etc.)"
echo "  5. Download GitHub Actions runner software"
echo ""
echo "After this, you'll:"
echo "  - Register the runner with GitHub (one command)"
echo "  - Add 6-8 config secrets to GitHub"
echo "  - Run E2E tests WITHOUT any Azure credentials in GitHub!"
echo ""

if ! confirm_action "Continue?"; then
    exit 0
fi

# Validate
validate_azure_login || exit 1
validate_required_vars AZURE_SUBSCRIPTION_ID RUNNER_RESOURCE_GROUP E2E_RESOURCE_GROUP || exit 1

print_config

# ============================================================================
# Step 0: Create Runner Resource Group (if needed)
# ============================================================================

print_header "Step 0: Ensure Runner Resource Group Exists"

if az group show --name "$RUNNER_RESOURCE_GROUP" &>/dev/null; then
    print_info "Resource group already exists: $RUNNER_RESOURCE_GROUP"
else
    print_step "Creating resource group '$RUNNER_RESOURCE_GROUP' in '$RUNNER_LOCATION'..."
    az group create \
        --name "$RUNNER_RESOURCE_GROUP" \
        --location "$RUNNER_LOCATION" \
        --tags "purpose=github-runner" "project=aksflexnode" \
        --output none
    print_success "Resource group created: $RUNNER_RESOURCE_GROUP"
fi

# ============================================================================
# Step 1: Create Runner VM with Managed Identity
# ============================================================================

print_header "Step 1: Create Runner VM (3-5 minutes)"

if az vm show -g "$RUNNER_RESOURCE_GROUP" -n "$RUNNER_VM_NAME" &>/dev/null; then
    print_warning "VM already exists: $RUNNER_VM_NAME"
    RUNNER_PUBLIC_IP=$(az vm show -g "$RUNNER_RESOURCE_GROUP" -n "$RUNNER_VM_NAME" --query "publicIps" -o tsv)
    print_info "Using existing VM: $RUNNER_PUBLIC_IP"
else
    print_step "Creating VM with Managed Identity..."

    az vm create \
        --resource-group "$RUNNER_RESOURCE_GROUP" \
        --name "$RUNNER_VM_NAME" \
        --location "$RUNNER_LOCATION" \
        --image Canonical:0001-com-ubuntu-server-jammy:22_04-lts-gen2:latest \
        --size "$RUNNER_VM_SIZE" \
        --admin-username "$RUNNER_USER" \
        --generate-ssh-keys \
        --public-ip-sku Standard \
        --assign-identity \
        --tags "purpose=github-runner" "project=aksflexnode" \
        --output json > /tmp/runner-vm.json

    RUNNER_PUBLIC_IP=$(jq -r '.publicIpAddress' /tmp/runner-vm.json)
    print_success "VM created: $RUNNER_PUBLIC_IP"
fi

# Get MSI Principal ID
MSI_PRINCIPAL_ID=$(az vm show \
    -g "$RUNNER_RESOURCE_GROUP" \
    -n "$RUNNER_VM_NAME" \
    --query "identity.principalId" -o tsv)

print_info "Managed Identity: $MSI_PRINCIPAL_ID"

# Save to .env
update_env_var "RUNNER_RESOURCE_GROUP" "$RUNNER_RESOURCE_GROUP"
update_env_var "RUNNER_LOCATION" "$RUNNER_LOCATION"
update_env_var "RUNNER_VM_NAME" "$RUNNER_VM_NAME"
update_env_var "RUNNER_PUBLIC_IP" "$RUNNER_PUBLIC_IP"
update_env_var "MSI_PRINCIPAL_ID" "$MSI_PRINCIPAL_ID"

# ============================================================================
# Step 2: Grant MSI Proper Permissions
# ============================================================================

print_header "Step 2: Grant MSI Permissions (1 minute)"

# Function to assign role (reusable)
assign_role() {
    local role="$1"
    local scope="$2"
    local desc="$3"

    if az role assignment list --assignee "$MSI_PRINCIPAL_ID" --role "$role" --scope "$scope" --query "[0].id" -o tsv 2>/dev/null | grep -q "/"; then
        print_warning "$desc - already assigned"
    else
        print_step "Assigning: $desc"
        az role assignment create --assignee "$MSI_PRINCIPAL_ID" --role "$role" --scope "$scope" --output none
        print_success "$desc - assigned"
    fi
}

# Assign all needed roles
assign_role "Contributor" \
    "/subscriptions/$AZURE_SUBSCRIPTION_ID/resourceGroups/$E2E_RESOURCE_GROUP" \
    "Contributor (create/delete VMs)"

assign_role "Azure Connected Machine Onboarding" \
    "/subscriptions/$AZURE_SUBSCRIPTION_ID/resourceGroups/$E2E_RESOURCE_GROUP" \
    "Arc Onboarding (register Arc machines)"

assign_role "User Access Administrator" \
    "/subscriptions/$AZURE_SUBSCRIPTION_ID/resourceGroups/$E2E_RESOURCE_GROUP" \
    "User Access Admin (grant permissions to test VMs)"

print_success "All permissions granted to runner MSI"
sleep 10  # Wait for propagation

# ============================================================================
# Step 3: Install Prerequisites on Runner VM
# ============================================================================

print_header "Step 3: Install Prerequisites (5-8 minutes)"

print_info "Waiting for SSH..."
sleep 30

for i in {1..10}; do
    if ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 ${RUNNER_USER}@${RUNNER_PUBLIC_IP} "echo ready" &>/dev/null; then
        break
    fi
    echo "Waiting for SSH... ($i/10)"
    sleep 10
done

print_step "Installing Azure CLI, kubectl, git, Docker..."

ssh -o StrictHostKeyChecking=no ${RUNNER_USER}@${RUNNER_PUBLIC_IP} 'bash -s' <<'REMOTE'
set -euo pipefail

echo "[1/5] Update system..."
sudo apt-get update -qq

echo "[2/5] Install dependencies..."
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y curl jq git wget ca-certificates apt-transport-https -qq

echo "[3/5] Install Azure CLI..."
curl -sL https://aka.ms/InstallAzureCLIDeb | sudo bash >/dev/null 2>&1

echo "[4/5] Install kubectl..."
curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl"
sudo install -o root -g root -m 0755 kubectl /usr/local/bin/kubectl
rm kubectl

echo "[5/5] Login with Managed Identity..."
az login --identity --output none

# Also login as github-runner user (who runs the workflows)
sudo su - github-runner -c "az login --identity --output none"

echo ""
echo "✅ All prerequisites installed"
az account show --query "{Subscription:name, AuthMethod:user.type}" -o table
REMOTE

print_success "Prerequisites installed"

# ============================================================================
# Step 4: Install GitHub Actions Runner Software
# ============================================================================

print_header "Step 4: Install GitHub Actions Runner (2 minutes)"

print_step "Downloading and installing runner software..."

ssh -o StrictHostKeyChecking=no ${RUNNER_USER}@${RUNNER_PUBLIC_IP} 'bash -s' <<'REMOTE'
set -euo pipefail

# Create runner user
if ! id github-runner &>/dev/null; then
    sudo useradd -m -s /bin/bash github-runner
    sudo usermod -aG sudo github-runner
    echo "github-runner ALL=(ALL) NOPASSWD:ALL" | sudo tee /etc/sudoers.d/github-runner >/dev/null
fi

# Download runner as github-runner user
sudo su - github-runner -c '
if [ ! -d "actions-runner" ]; then
    mkdir actions-runner && cd actions-runner
    RUNNER_VERSION="2.331.0"
    curl -sL -o actions-runner-linux-x64-${RUNNER_VERSION}.tar.gz \
        https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-x64-${RUNNER_VERSION}.tar.gz
    tar xzf ./actions-runner-linux-x64-${RUNNER_VERSION}.tar.gz
    echo "✅ Runner software installed"
else
    echo "✅ Runner software already installed"
fi
'
REMOTE

print_success "Runner software ready for registration"

# ============================================================================
# Summary and Next Steps
# ============================================================================

print_header "✅ Runner VM Setup Complete!"

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "📋 RUNNER VM DETAILS"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "Resource Group:       $RUNNER_RESOURCE_GROUP"
echo "Location:             $RUNNER_LOCATION"
echo "VM Name:              $RUNNER_VM_NAME"
echo "Public IP:            $RUNNER_PUBLIC_IP"
echo "Managed Identity ID:  $MSI_PRINCIPAL_ID"
echo ""
echo "Permissions Granted:"
echo "  ✅ Contributor (on E2E test resource group: $E2E_RESOURCE_GROUP)"
echo "  ✅ Azure Connected Machine Onboarding (on E2E test resource group)"
echo "  ✅ User Access Administrator (on E2E test resource group)"
echo ""
echo "Note: Runner can create clusters/VMs and grant permissions to test VMs"
echo ""
echo "Prerequisites Installed:"
echo "  ✅ Azure CLI (logged in with MSI)"
echo "  ✅ kubectl"
echo "  ✅ Git, jq, curl"
echo "  ✅ GitHub Actions runner software (ready to register)"
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "🎯 NEXT STEP: Register Runner with GitHub"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "1. Get registration token from GitHub:"
echo "   https://github.com/${GITHUB_ORG}/${GITHUB_REPO}/settings/actions/runners/new"
echo ""
echo "2. Run the registration script:"
echo "   ./scripts/setup/register-runner.sh <TOKEN>"
echo ""
echo "   Or manually SSH and register:"
echo "   ssh ${RUNNER_USER}@${RUNNER_PUBLIC_IP}"
echo "   sudo su - github-runner"
echo "   cd actions-runner"
echo "   ./config.sh --url https://github.com/${GITHUB_ORG}/${GITHUB_REPO} --token <TOKEN>"
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "🔐 GITHUB SECRETS NEEDED"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "Add these 4 configuration secrets to GitHub:"
echo "  https://github.com/${GITHUB_ORG}/${GITHUB_REPO}/settings/secrets/actions"
echo ""
echo "E2E_RESOURCE_GROUP:      $E2E_RESOURCE_GROUP"
echo "E2E_LOCATION:            $E2E_LOCATION"
echo "AZURE_SUBSCRIPTION_ID:   $AZURE_SUBSCRIPTION_ID"
echo "AZURE_TENANT_ID:         $AZURE_TENANT_ID"
echo ""
echo "Note: Test VMs use their own Managed Identity - no Service Principal needed!"
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
