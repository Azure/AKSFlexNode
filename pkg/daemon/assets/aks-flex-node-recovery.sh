#!/bin/bash
set -euo pipefail

signal=/etc/aks-flex-node/agent-upgrade-signal.json
last_good="$(readlink -f /usr/local/lib/aks-flex-node/aks-flex-node-last-good || true)"

# An ordinary daemon failure must not change the selected binary.
if [[ ! -f "${signal}" ]]; then
    exit 0
fi
if [[ -z "${last_good}" || ! -x "${last_good}" ]]; then
    echo "no executable last-known-good AKS Flex Node agent binary" >&2
    exit 1
fi

"${last_good}" recover-agent-upgrade \
    --message "upgraded daemon failed repeatedly; restored last-good binary"
systemctl reset-failed aks-flex-node-agent.service
systemctl --no-block restart aks-flex-node-agent.service
