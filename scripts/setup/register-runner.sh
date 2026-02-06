#!/bin/bash
# Complete runner registration - Run this after getting the token from GitHub

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

source "$SCRIPT_DIR/common.sh"
cd "$PROJECT_ROOT"
load_env ".env" || exit 1

# ============================================================================
# Configuration
# ============================================================================

if [ -z "${1:-}" ]; then
    print_error "Usage: $0 <GITHUB_TOKEN>"
    echo ""
    echo "Get token from:"
    echo "  https://github.com/${GITHUB_ORG}/${GITHUB_REPO}/settings/actions/runners/new"
    echo ""
    echo "The token looks like: A...XXXXX (alphanumeric string)"
    exit 1
fi

GITHUB_TOKEN="$1"

print_header "Completing GitHub Actions Runner Registration"

print_info "Runner VM: $RUNNER_VM_NAME"
print_info "Runner IP: $RUNNER_PUBLIC_IP"
print_info "GitHub Repo: ${GITHUB_ORG}/${GITHUB_REPO}"

# ============================================================================
# Register and Start Runner
# ============================================================================

print_step "Configuring runner on VM..."

ssh -o StrictHostKeyChecking=no azureuser@${RUNNER_PUBLIC_IP} <<REMOTE_EOF
set -euo pipefail

echo "Configuring GitHub Actions runner..."

# Configure as github-runner user
sudo su - github-runner -c "
cd actions-runner
./config.sh \
  --url https://github.com/${GITHUB_ORG}/${GITHUB_REPO} \
  --token ${GITHUB_TOKEN} \
  --name aksflexnode-e2e-runner \
  --labels e2e,azure,ubuntu \
  --work _work \
  --unattended
"

echo "✅ Runner configured"

# Install and start service
echo "Installing runner as systemd service..."
cd /home/github-runner/actions-runner
sudo ./svc.sh install github-runner
sudo ./svc.sh start

echo ""
echo "Checking service status..."
sudo ./svc.sh status

echo ""
echo "✅ Runner service started"

REMOTE_EOF

print_success "Runner registration complete!"

echo ""
print_header "✅ Self-Hosted Runner Setup Complete!"
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "📋 RUNNER DETAILS"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "Runner Name:    aksflexnode-e2e-runner"
echo "Runner VM:      $RUNNER_VM_NAME"
echo "Runner IP:      $RUNNER_PUBLIC_IP"
echo "Status:         Should show 'Idle' in GitHub"
echo ""
echo "Verify at:"
echo "  https://github.com/${GITHUB_ORG}/${GITHUB_REPO}/settings/actions/runners"
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "✅ NEXT STEPS"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "1. Update .github/workflows/e2e-tests.yml:"
echo "   Change 'runs-on: ubuntu-latest' to 'runs-on: self-hosted'"
echo ""
echo "2. Add GitHub Secrets (configuration only, no Azure credentials!):"
echo "   - E2E_RESOURCE_GROUP: $E2E_RESOURCE_GROUP"
echo "   - E2E_AKS_CLUSTER_NAME: $E2E_AKS_CLUSTER_NAME"
echo "   - E2E_AKS_RESOURCE_ID: $E2E_AKS_RESOURCE_ID"
echo "   - E2E_LOCATION: $E2E_LOCATION"
echo "   - AZURE_SUBSCRIPTION_ID: $AZURE_SUBSCRIPTION_ID"
echo "   - AZURE_TENANT_ID: $AZURE_TENANT_ID"
echo ""
echo "3. Create test VM service principal (Azure Portal):"
echo "   - For the config.json that test VMs will use"
echo "   - Add E2E_VM_CLIENT_ID and E2E_VM_CLIENT_SECRET to GitHub"
echo ""
echo "4. Test the runner:"
echo "   gh workflow run e2e-tests.yml"
echo ""
echo "🎉 Your runner will use Managed Identity - no AZURE_CREDENTIALS secret needed!"
echo ""
