#!/bin/bash
# FlexNode agent ↔ RP integration test
#
# Prerequisites:
#   - kubectl port-forward already running:
#     kubectl --kubeconfig /home/hayua/.kube/hcphayuaebld158399352-eastus2-svc-0-kubeconfig \
#       port-forward -n containerservice svc/containerserviceinternal-stable 18081:4000
#
# Usage:
#   ./test_rp_integration.sh [PORT] [MACHINE_NAME]

set -euo pipefail

LOCAL_PORT="${1:-18081}"
MACHINE="${2:-docker-vm-$(date +%s | tail -c 5)}"
SUBSCRIPTION="8ecadfc9-d1a3-4ea4-b844-0d9f87e4d7c8"
RG="flexnode-test-35468"
CLUSTER="fntest35468"

GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m'

cd /home/hayua/AKSFlexNode

# --- Build ---
echo -e "${YELLOW}Building rptest...${NC}"
CGO_ENABLED=0 GOOS=linux go build -o rptest ./cmd/rptest/
docker build -f Dockerfile.rptest -t flexnode-rptest:latest . -q
echo -e "${GREEN}✓ Built${NC}"

# --- Smoke test ---
echo -e "${YELLOW}Checking RP at localhost:${LOCAL_PORT}...${NC}"
HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" --max-time 10 \
    "http://localhost:${LOCAL_PORT}/subscriptions/${SUBSCRIPTION}/resourceGroups/${RG}/providers/Microsoft.ContainerService/managedClusters/${CLUSTER}/agentPools/flexnode/machines?api-version=2026-03-02-preview")
if [ "$HTTP_CODE" = "200" ]; then
    echo -e "${GREEN}✓ RP reachable${NC}"
else
    echo -e "${RED}✗ RP returned HTTP $HTTP_CODE — is port-forward running?${NC}"
    echo "  kubectl --kubeconfig /home/hayua/.kube/hcphayuaebld158399352-eastus2-svc-0-kubeconfig \\"
    echo "    port-forward -n containerservice svc/containerserviceinternal-stable ${LOCAL_PORT}:4000"
    exit 1
fi

# --- Run from container ---
echo -e "${YELLOW}Running test in container (machine: $MACHINE)${NC}"
echo ""

docker run --rm --network=host flexnode-rptest:latest \
    --rp "http://localhost:${LOCAL_PORT}" \
    --sub "$SUBSCRIPTION" \
    --rg "$RG" \
    --cluster "$CLUSTER" \
    --machine "$MACHINE"
