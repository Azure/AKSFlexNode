# Repave Flow

This document describes the current AKS Flex Node repave flow.

## Overview

AKS Flex Node no longer runs a standalone local drift detector. Desired node settings come from an AKS machine resource. The agent compares the desired machine goal with locally persisted daemon state and repaves the nspawn-backed worker when Kubernetes `Node` deletion indicates AKS has approved replacement.

The current machine goal includes:

- `kubernetesVersion`
- `settingsVersion`

`settingsVersion` is the drift key. If it differs from locally applied state, the agent waits for the Kubernetes `Node` object to disappear before mutating host state.

## Inputs

The daemon uses two inputs:

- Desired state from the AKS machine client.
- Applied state persisted locally by the daemon.

In E2E mode, the machine client is file-backed at `/run/aks-flex-node/e2e-machine.json` so tests can simulate AKS RP machine updates without production ARM integration.

## Scheduling

The daemon reconciles machine state on startup and on `agent.machineReconcileInterval`. E2E configs use `agent.e2eMode` to select the local file-backed machine client.

Production AKS RP machine client integration is still pending. Until then, non-E2E daemon mode blocks after startup and bootstrap uses a no-op machine client.

## Repave Trigger

Repave requires both conditions:

- The machine goal differs from the locally applied daemon state.
- The Kubernetes `Node` object for the current nspawn side is absent.

This keeps scheduling and disruption decisions outside the agent. AKS RP, an operator, or an E2E helper updates the machine goal and deletes the Kubernetes `Node`; the daemon reacts by applying the new goal.

## Repave Flow

`daemon.NSpawnNodeOperator.ApplyGoalState` performs the nspawn side replacement:

1. Load the active side from persisted daemon state (`ActiveMachine`).
2. Select the alternate side with `goalstates.AlternateMachine`.
3. Resolve unbounded machine goal state for the alternate side.
4. Provision the alternate rootfs.
5. Apply AKS-specific rootfs customization.
6. Stop the old nspawn side.
7. Start the new nspawn side.
8. Wait for kubelet to become active inside the new side.
9. Start node-problem-detector inside the new side.
10. Clean up the old side's nspawn artifacts.

After successful repave, the daemon patches machine status and persists the applied goal locally.

## Active Machine Discovery

AKS Flex Node uses two local nspawn machine names:

- `kube1`
- `kube2`

The initial bootstrap starts `kube1` and seeds daemon state with `ActiveMachine: kube1`. Repave loads `ActiveMachine` from persisted daemon state, treats that value as the old side, then provisions and starts the alternate side. Runtime `machinectl` state is not the source of truth for side selection.

## E2E Coverage

The default E2E `all` flow includes local-machine-driven repave coverage for MSI, bootstrap-token, and kubeadm modes. The suite also exposes an explicit `upgrade-drift` command for running only the repave scenario after infra is deployed.

The flow updates the local E2E machine goal, deletes the Kubernetes `Node`, waits for the daemon to repave to `kube2`, validates the host kubelet and Kubernetes Node-reported kubelet major/minor match the AKS desired version, and runs a smoke pod.

Run it after infra is deployed:

```bash
./hack/e2e/run.sh infra
./hack/e2e/run.sh upgrade-drift
```

Optional override:

```bash
E2E_DRIFT_UPGRADE_TIMEOUT=1200 ./hack/e2e/run.sh upgrade-drift
```

## Current Limitations

- Production AKS RP machine client implementation is pending.
- Rollback is not yet automatic if failure happens after the old side is stopped but before the new side is fully healthy.
- Active side selection depends on persisted daemon state. If that state file is missing, corrupt, or stale, repave cannot safely choose the old side and should require operator intervention instead of guessing from runtime `machinectl` state.
