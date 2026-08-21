#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
WORK_DIR="$(mktemp -d)"
trap 'rm -rf "${WORK_DIR}"' EXIT

mkdir -p "${WORK_DIR}/bin"
cat > "${WORK_DIR}/bin/az" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

query=""
for ((index = 1; index <= $#; index++)); do
  if [[ "${!index}" == "--query" ]]; then
    next=$((index + 1))
    query="${!next}"
    break
  fi
done

case "$1 $2|${query}" in
  "account show|id") printf '%s\n' "00000000-0000-0000-0000-000000000001" ;;
  "account show|tenantId") printf '%s\n' "00000000-0000-0000-0000-000000000002" ;;
  "aks show|id") printf '%s\n' "/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/parity-rg/providers/Microsoft.ContainerService/managedClusters/parity-cluster" ;;
  "aks show|location") printf '%s\n' "westus2" ;;
  "aks show|currentKubernetesVersion || kubernetesVersion") printf '%s\n' "1.35.0" ;;
  "aks show|networkProfile.dnsServiceIp") printf '%s\n' "10.0.0.10" ;;
  *)
    printf 'unexpected az invocation: %q ' "$@" >&2
    printf '\n' >&2
    exit 1
    ;;
esac
EOF
chmod +x "${WORK_DIR}/bin/az"

cat > "${WORK_DIR}/expected.json" <<'EOF'
{
  "azure": {
    "subscriptionId": "00000000-0000-0000-0000-000000000001",
    "tenantId": "00000000-0000-0000-0000-000000000002",
    "resourceManagerEndpoint": "https://management.azure.com",
    "targetAgentPoolName": "paritypool",
    "targetCluster": {
      "resourceId": "/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/parity-rg/providers/Microsoft.ContainerService/managedClusters/parity-cluster",
      "location": "westus2"
    },
    "managedIdentity": {
      "clientId": "00000000-0000-0000-0000-000000000003"
    },
    "arc": {
      "enabled": false
    }
  },
  "agent": {
    "logLevel": "info",
    "logDir": "/var/log/aks-flex-node"
  },
  "components": {
    "kubernetes": "1.35.0"
  }
}
EOF

go build -o "${WORK_DIR}/aks-flex-config" "${REPO_ROOT}/cmd/aks-flex-config"

common_args=(
  generate-node-config
  --resource-group parity-rg
  --cluster-name parity-cluster
  --subscription 00000000-0000-0000-0000-000000000001
  --agent-pool-name paritypool
  --identity
  --username 00000000-0000-0000-0000-000000000003
)

PATH="${WORK_DIR}/bin:${PATH}" python3 "${REPO_ROOT}/scripts/aks-flex-config" \
  "${common_args[@]}" --output "${WORK_DIR}/python.json"
PATH="${WORK_DIR}/bin:${PATH}" "${WORK_DIR}/aks-flex-config" \
  "${common_args[@]}" --output "${WORK_DIR}/binary.json"

for name in expected python binary; do
  jq --sort-keys --compact-output . "${WORK_DIR}/${name}.json" > "${WORK_DIR}/${name}.canonical.json"
done

diff -u "${WORK_DIR}/expected.canonical.json" "${WORK_DIR}/python.canonical.json"
diff -u "${WORK_DIR}/expected.canonical.json" "${WORK_DIR}/binary.canonical.json"
diff -u "${WORK_DIR}/python.canonical.json" "${WORK_DIR}/binary.canonical.json"

echo "aks-flex-config Python and native binary outputs match the expected config"