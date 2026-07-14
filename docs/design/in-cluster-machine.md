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
        "kubernetes.azure.com/managed": "false"
      }
    },
    "provisioningState": "Succeeded"
  }
}
```

Status updates use a separate patch model because the agent operation status is not represented by the current ARM SDK machine model. A status-only patch must not change the machine ETag.

## Bootstrap flow

The local bootstrap configuration is authoritative while `aks-flex-node start` is running:

1. `NewMachineClient` selects the in-cluster backend without a supplied Kubernetes REST config.
2. The client builds a REST config from the bootstrap token or configured exec credential.
3. `EnsureMachine` reads the machine through the Kubernetes service proxy.
4. If the machine is absent, the client sends a PUT using the local bootstrap goal.
5. If its Kubernetes version differs, the client sends a PUT that overwrites the remote goal with the local version.
6. If its Kubernetes version already matches, local bootstrap settings remain authoritative; remote settings other than the ETag do not replace them.
7. The returned ETag becomes the reconciliation baseline for the locally applied goal.
8. The daemon state is seeded from that ETag before host or nspawn state is mutated. A later ETag change is treated as a new remote goal.

The ConfigMap-backed controller is read-only: it accepts mutation requests but returns the pre-created machine. Its fixture must therefore already match the local bootstrap version. When machine registration is required, a mismatch fails bootstrap before host mutation.

## Daemon flow

After bootstrap, the remote machine is authoritative:

1. The daemon obtains its long-lived Kubernetes REST config, including certificate rotation when bootstrap-token authentication is configured.
2. `NewMachineClient` receives that REST config and selects the in-cluster backend.
3. The client periodically reads the ARM-compatible machine through the service-proxy endpoint.
4. The daemon compares `properties.eTag` with its locally applied settings version.
5. A changed ETag represents a new goal. The daemon waits for the Kubernetes `Node` deletion signal before applying it.
6. Reconciliation status is sent to the endpoint's `/status` subresource without changing the ETag.

## Request path

For the default deployment, requests use the Kubernetes API service proxy:

```text
/api/v1/namespaces/kube-system/services/http:aks-flex-controller:80/proxy/machines/<node-name>
```

Bootstrap requests use bootstrap credentials because daemon credentials do not exist yet. Long-running daemon requests use the daemon REST config instead of retaining the bootstrap credential.
