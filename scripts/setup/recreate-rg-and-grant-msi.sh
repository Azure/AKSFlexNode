#!/bin/bash
#
# Recreate Resource Groups and Grant MSI Permissions
#
# This script recreates deleted resource groups and grants necessary permissions
# to the runner VM's Managed Identity.
#

set -euo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo -e "${BLUE}🔧 AKSFlexNode - Recreate Resource Groups & Grant MSI Permissions${NC}"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

# Check if .env file exists
if [ ! -f ".env" ]; then
    echo -e "${RED}❌ Error: .env file not found${NC}"
    echo ""
    echo "Please create a .env file from .env.example:"
    echo "  cp .env.example .env"
    echo "  # Edit .env and fill in your values"
    exit 1
fi

# Load environment variables
echo -e "${BLUE}📋 Loading configuration from .env...${NC}"
set -a
source .env
set +a

# Validate required variables
REQUIRED_VARS=(
    "AZURE_SUBSCRIPTION_ID"
    "AZURE_TENANT_ID"
    "E2E_RESOURCE_GROUP"
    "E2E_LOCATION"
    "RUNNER_RESOURCE_GROUP"
    "RUNNER_VM_NAME"
)

for var in "${REQUIRED_VARS[@]}"; do
    if [ -z "${!var:-}" ]; then
        echo -e "${RED}❌ Error: $var is not set in .env${NC}"
        exit 1
    fi
done

echo -e "${GREEN}✅ Configuration loaded${NC}"
echo ""

# Display configuration
echo "Configuration:"
echo "  Subscription ID:       $AZURE_SUBSCRIPTION_ID"
echo "  Tenant ID:             $AZURE_TENANT_ID"
echo "  E2E Resource Group:    $E2E_RESOURCE_GROUP"
echo "  E2E Location:          $E2E_LOCATION"
echo "  Runner Resource Group: $RUNNER_RESOURCE_GROUP"
echo "  Runner VM Name:        $RUNNER_VM_NAME"
echo ""

# Verify Azure login
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo -e "${BLUE}🔐 Verifying Azure Authentication${NC}"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

if ! az account show &>/dev/null; then
    echo -e "${RED}❌ Not logged in to Azure${NC}"
    echo ""
    echo "Please login first:"
    echo "  az login"
    exit 1
fi

CURRENT_SUB=$(az account show --query id -o tsv)
if [ "$CURRENT_SUB" != "$AZURE_SUBSCRIPTION_ID" ]; then
    echo -e "${YELLOW}⚠️  Current subscription doesn't match .env${NC}"
    echo "  Current:  $CURRENT_SUB"
    echo "  Expected: $AZURE_SUBSCRIPTION_ID"
    echo ""
    echo "Switching to correct subscription..."
    az account set --subscription "$AZURE_SUBSCRIPTION_ID"
fi

echo -e "${GREEN}✅ Authenticated as:${NC}"
az account show --query "{Subscription:name, User:user.name}" -o table
echo ""

# Get Runner VM MSI
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo -e "${BLUE}🔍 Getting Runner VM Managed Identity${NC}"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

# Check if runner RG exists
if ! az group show --name "$RUNNER_RESOURCE_GROUP" &>/dev/null; then
    echo -e "${RED}❌ Error: Runner resource group '$RUNNER_RESOURCE_GROUP' does not exist${NC}"
    echo ""
    echo "The runner VM must exist before running this script."
    echo "Please create the runner VM first using:"
    echo "  ./scripts/setup/setup-runner.sh"
    exit 1
fi

# Check if runner VM exists
if ! az vm show --resource-group "$RUNNER_RESOURCE_GROUP" --name "$RUNNER_VM_NAME" &>/dev/null; then
    echo -e "${RED}❌ Error: Runner VM '$RUNNER_VM_NAME' does not exist in '$RUNNER_RESOURCE_GROUP'${NC}"
    echo ""
    echo "Please create the runner VM first using:"
    echo "  ./scripts/setup/setup-runner.sh"
    exit 1
fi

# Get MSI principal ID
MSI_PRINCIPAL_ID=$(az vm show \
    --resource-group "$RUNNER_RESOURCE_GROUP" \
    --name "$RUNNER_VM_NAME" \
    --query "identity.principalId" \
    -o tsv)

if [ -z "$MSI_PRINCIPAL_ID" ] || [ "$MSI_PRINCIPAL_ID" == "null" ]; then
    echo -e "${RED}❌ Error: Runner VM does not have a system-assigned managed identity${NC}"
    echo ""
    echo "Please enable system-assigned identity on the VM:"
    echo "  az vm identity assign --resource-group $RUNNER_RESOURCE_GROUP --name $RUNNER_VM_NAME"
    exit 1
fi

echo -e "${GREEN}✅ Found Runner VM MSI:${NC}"
echo "  VM Name:      $RUNNER_VM_NAME"
echo "  Principal ID: $MSI_PRINCIPAL_ID"
echo ""

# Recreate E2E Resource Group
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo -e "${BLUE}📦 Creating E2E Resource Group${NC}"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

if az group show --name "$E2E_RESOURCE_GROUP" &>/dev/null; then
    echo -e "${YELLOW}⚠️  Resource group '$E2E_RESOURCE_GROUP' already exists${NC}"
    echo ""
    read -p "Do you want to continue without recreating it? (y/n) " -n 1 -r
    echo ""
    if [[ ! $REPLY =~ ^[Yy]$ ]]; then
        echo "Aborted."
        exit 1
    fi
else
    echo "Creating resource group: $E2E_RESOURCE_GROUP"
    echo "Location: $E2E_LOCATION"
    echo ""

    az group create \
        --name "$E2E_RESOURCE_GROUP" \
        --location "$E2E_LOCATION" \
        --tags "purpose=e2e-testing" "project=aksflexnode" \
        --output none

    echo -e "${GREEN}✅ Resource group created: $E2E_RESOURCE_GROUP${NC}"
fi
echo ""

# Grant MSI Permissions
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo -e "${BLUE}🔑 Granting MSI Permissions${NC}"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

SUBSCRIPTION_SCOPE="/subscriptions/$AZURE_SUBSCRIPTION_ID"
E2E_RG_SCOPE="/subscriptions/$AZURE_SUBSCRIPTION_ID/resourceGroups/$E2E_RESOURCE_GROUP"

# Required roles for the runner VM's MSI
declare -A ROLES=(
    ["Contributor"]="$E2E_RG_SCOPE"
    ["User Access Administrator"]="$E2E_RG_SCOPE"
    ["Azure Kubernetes Service Cluster Admin Role"]="$SUBSCRIPTION_SCOPE"
    ["Azure Kubernetes Service RBAC Cluster Admin"]="$SUBSCRIPTION_SCOPE"
)

echo "Granting permissions to runner MSI..."
echo ""

for role in "${!ROLES[@]}"; do
    scope="${ROLES[$role]}"

    echo -e "${BLUE}[$role]${NC}"
    echo "  Scope: $scope"

    # Check if role assignment already exists
    if az role assignment list \
        --assignee "$MSI_PRINCIPAL_ID" \
        --role "$role" \
        --scope "$scope" \
        --query "[0].id" -o tsv 2>/dev/null | grep -q "."; then
        echo -e "  ${YELLOW}⚠️  Role already assigned (skipping)${NC}"
    else
        # Create role assignment
        az role assignment create \
            --assignee-object-id "$MSI_PRINCIPAL_ID" \
            --assignee-principal-type ServicePrincipal \
            --role "$role" \
            --scope "$scope" \
            --output none

        echo -e "  ${GREEN}✅ Role assigned${NC}"
    fi
    echo ""
done

# Wait for permission propagation
echo -e "${BLUE}⏳ Waiting 15s for permission propagation...${NC}"
sleep 15
echo ""

# Summary
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo -e "${GREEN}✅ Setup Complete!${NC}"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "Summary:"
echo "  ✅ E2E Resource Group: $E2E_RESOURCE_GROUP (created/verified)"
echo "  ✅ Runner VM MSI: $MSI_PRINCIPAL_ID"
echo "  ✅ Permissions granted to E2E resource group"
echo "  ✅ Permissions granted at subscription level for AKS operations"
echo ""
echo "The runner VM can now:"
echo "  • Create and delete AKS clusters in $E2E_RESOURCE_GROUP"
echo "  • Create and delete VMs in $E2E_RESOURCE_GROUP"
echo "  • Manage AKS cluster permissions"
echo "  • Run E2E tests"
echo ""
echo "Next steps:"
echo "  1. Verify permissions: az role assignment list --assignee $MSI_PRINCIPAL_ID"
echo "  2. Test E2E workflow: gh workflow run e2e-tests.yml"
echo ""
