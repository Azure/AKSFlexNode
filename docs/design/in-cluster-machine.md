# In-Cluster Machine Flow

The in-cluster machine endpoint lets FlexNode exercise the same machine lifecycle used by ARM while authenticating through the Kubernetes API server. It is intended for E2E and dev-test environments; direct ARM remains the production backend.

## Machine contract

The controller serves the `armcontainerservice.Machine` JSON shape from the `kube-system/aks-flex-machines` ConfigMap. Desired node settings come from `properties.kubernetes`, and `properties.eTag` is the opaque settings version used for drift detection.

```json
{
  "name": "flex-node-1",
  "properties": {
    "eTag": "settings-42",
    "kubernetes": {
      "orchestratorVersion": "1.34.0",
      "maxPods": 110,
      "nodeLabels": {
        "workload": "edge"
      }
    },
    "provisioningState": "Succeeded"
  }
}
```

Status updates use a separate patch model because the agent operation status is not represented by the current ARM SDK machine model. A status-only patch must not change the machine ETag.

## Bootstrap flow

The local bootstrap configuration seeds a Machine when one does not already exist. Once the endpoint returns a Machine, its complete goal is authoritative for bootstrap:

1. `NewMachineClient` selects the in-cluster backend without a supplied Kubernetes REST config.
2. The client builds a REST config from the bootstrap token or configured exec credential.
3. `EnsureMachine` reads the machine through the Kubernetes service proxy.
4. If the machine is absent, the client sends a PUT using the local bootstrap goal.
5. Whether read or created, the returned Machine is validated and its goal replaces the local bootstrap goal. This includes Kubernetes version, max pods, custom labels, taints, kubelet image-GC thresholds, and the ETag-backed settings version. Scalar defaults omitted by the API retain their validated local bootstrap values.
6. The daemon resolves nspawn settings and seeds its state from that same effective goal before mutating the host. A later ETag change is treated as a new remote goal.

When `orchestratorVersion` is a `major.minor` alias, the returned `currentOrchestratorVersion` supplies the exact patch used for artifact resolution.

The ConfigMap-backed controller is read-only: it accepts mutation requests but returns the pre-created Machine. The agent adopts that returned goal even when it differs from local bootstrap configuration. When registration is required, a read, create, or validation failure stops bootstrap before host mutation. When registration is optional, bootstrap continues with the local goal.

## Daemon flow

After bootstrap, the remote machine is authoritative:

1. The daemon obtains its long-lived Kubernetes REST config, including certificate rotation when bootstrap-token authentication is configured.
2. `NewMachineClient` receives that REST config and selects the in-cluster backend.
3. The client periodically reads the ARM-compatible machine through the service-proxy endpoint.
4. The daemon compares `properties.eTag` with its locally applied settings version.
5. A changed ETag represents a new goal. If only labels or taints changed and AKS RP already reconciled them onto the existing Kubernetes `Node`, the daemon acknowledges the observed goal without mutating or repaving the node.
6. Other goal changes wait for the Kubernetes `Node` deletion signal before the daemon applies them through blue-green repave.
7. Reconciliation status is sent to the endpoint's `/status` subresource without changing the ETag.

Direct ARM Machine status is currently read-only. In that mode, acknowledgement still advances the local applied goal and ETag, while the status mutation is skipped by the ARM client.

## Request path

For the default deployment, requests use the Kubernetes API service proxy:

```text
/api/v1/namespaces/kube-system/services/http:aks-flex-controller:80/proxy/machines/<node-name>
```

Bootstrap requests use bootstrap credentials because daemon credentials do not exist yet. Long-running daemon requests use the daemon REST config instead of retaining the bootstrap credential.
