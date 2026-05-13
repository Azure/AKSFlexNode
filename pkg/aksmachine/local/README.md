# Local AKS Machine Backend

This package provides a file-backed implementation of `aksmachine.MachineClient` for e2e tests.

It is not a production AKS RP or ARM implementation. Production code should use an SDK-backed client once the AKS machine ARM contract is available in the public Azure SDK. This local backend exists so e2e tests can simulate AKS RP machine goal-state changes by mutating a JSON file on disk.

## Purpose

The local backend lets tests validate agent behavior without depending on a live AKS RP endpoint.

It supports:

- Creating a local machine JSON file with a desired Kubernetes version and settings version.
- Reading the current local machine representation.
- Patching machine status.
- Simulating machine deletion by deleting the file.

When the file does not exist, `Get` returns `*aksmachine.NotFoundError`. This matches the reset/delete design where the agent treats a missing machine representation as an ARM 404 equivalent.

## File Format

The file stores the local `aksmachine.Machine` JSON shape:

```json
{
  "id": "local-test-machine",
  "goal": {
    "kubernetesVersion": "1.34.0",
    "settingsVersion": "42"
  },
  "status": {
    "provisioningState": "Succeeded",
    "observedSettingsVersion": "42",
    "message": ""
  }
}
```

The local backend always uses `local-test-machine` as the resource ID when creating the file.

## CLI Usage

Use the dedicated e2e helper binary, not the main `aks-flex-node` binary.

Build it with:

```bash
make build-e2ehelper
```

Create or replace the local machine goal state:

```bash
./e2ehelper local-machine create \
  --path /tmp/aks-machine.json \
  --kubernetes-version 1.34.0 \
  --settings-version 42
```

Read the current machine file:

```bash
./e2ehelper local-machine get --path /tmp/aks-machine.json
```

Patch status:

```bash
./e2ehelper local-machine status \
  --path /tmp/aks-machine.json \
  --provisioning-state Succeeded \
  --observed-settings-version 42 \
  --message "applied"
```

Delete the local machine representation:

```bash
./e2ehelper local-machine delete --path /tmp/aks-machine.json
```

## E2E Pattern

An e2e test can point the agent at this file-backed client, then trigger control-plane-like changes by mutating the file:

- Upgrade/reimage/rollback: update `goal.kubernetesVersion` and `goal.settingsVersion`, then trigger the Kubernetes node signal used by the scenario.
- Reset/delete: delete the file to simulate the ARM machine 404, then trigger the Kubernetes node annotation used by the scenario.

The backend writes JSON atomically through `utilio.WriteFile`, but e2e tests may also directly overwrite or delete the file when simulating external AKS RP behavior.
