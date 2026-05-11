# Drift Detection And Repave

This document describes the current AKS Flex Node drift detection and nspawn repave flow.

## Overview

AKS Flex Node periodically compares the desired AKS cluster state with the current local node state. When it detects actionable Kubernetes version drift, it remediates by repaving the nspawn-backed node onto the inactive machine side.

The current implementation supports one actionable drift type:

- Kubernetes kubelet major/minor version is older than the AKS managed cluster current Kubernetes version.

Patch-only version differences are ignored.

## Inputs

Drift detection uses two local runtime snapshots:

- Desired state: `/run/aks-flex-node/managedcluster-spec.json`
- Current state: `/run/aks-flex-node/status.json`

The managed cluster spec is collected from the AKS managed cluster resource. It includes `currentKubernetesVersion`, `kubernetesVersion`, `fqdn`, and collection metadata.

The node status snapshot is collected from the active nspawn machine. It includes kubelet version, kubelet running state, readiness, containerd/runc versions, Arc status, and update metadata.

## Scheduling

The daemon performs drift detection in two places:

- Once at daemon startup after initial managed cluster spec collection.
- Every 10 minutes in the managed cluster spec collection loop.

Status collection runs every minute. Bootstrap health checks run every two minutes.

Drift remediation and bootstrap health checks share an atomic `bootstrapInProgress` guard so only one host-mutating operation runs at a time.

## Detection

The default detector set currently contains `KubernetesVersionDetector`.

The detector compares:

- Desired version: `ManagedClusterSpec.CurrentKubernetesVersion`, falling back to `ManagedClusterSpec.KubernetesVersion`.
- Current version: `NodeStatus.KubeletVersion`.

Both versions are parsed with tolerant semver handling and normalized to major/minor by setting patch to zero. Automatic remediation is skipped if either version cannot be parsed.

The detector returns an actionable finding only when current major/minor is lower than desired major/minor. It never triggers downgrades.

## Planning

Detected findings are reduced to a single remediation plan.

The planner rejects conflicting remediation actions or conflicting desired Kubernetes versions. Informational findings with no remediation action are logged but do not trigger remediation.

Today, the only supported action is `kubernetes-upgrade`.

## Remediation Flow

Kubernetes upgrade remediation happens in `pkg/drift` and `pkg/bootstrapper`.

The high-level flow is:

1. Cordon and drain the Kubernetes node.
2. Repave the nspawn machine using blue/green sides.
3. Uncordon the node if this remediation cordoned it.
4. Invalidate the cached kubelet clientset.
5. Mark kubelet status healthy with the desired version.

If the repave step fails, drift marks kubelet unhealthy in the status snapshot so the daemon can surface the failure and future recovery logic can act on it.

## Active Machine Discovery

AKS Flex Node uses two local nspawn machine names:

- `kube1`
- `kube2`

The initial bootstrap starts `kube1`. Repave discovers the currently running side using host `machinectl` state, then provisions and starts the alternate side.

Unlike unbounded-agent, AKS Flex Node does not currently use persisted applied config as the desired state source. Desired state comes from fresh AKS/status snapshots before remediation. Active side discovery therefore uses runtime machine state only.

The same active-machine discovery is used by daemon status collection and bootstrap health checks so they follow the active side after a successful repave.

## Repave Flow

`bootstrapper.Repave` performs the nspawn side replacement:

1. Detect the single running side with `machinectl show <machine> --property=State --value`.
2. Select the alternate side with `goalstates.AlternateMachine`.
3. Resolve unbounded machine goal state for the alternate side.
4. Provision the alternate rootfs.
5. Apply AKS-specific rootfs customization:
   - Download node-problem-detector.
   - Copy the `aks-flex-node` binary into the rootfs.
   - Write the bridge CNI config.
6. Stop the old nspawn side.
7. Start the new nspawn side.
8. Wait for kubelet to become active inside the new side.
9. Start node-problem-detector inside the new side.
10. Clean up the old side's nspawn artifacts.

## Status And Recovery Behavior

After successful drift remediation, the status snapshot is updated best-effort to mark kubelet running and set kubelet version to the desired Kubernetes version.

After a node repave failure, the status snapshot is updated best-effort to mark kubelet unhealthy:

- `KubeletRunning = false`
- `KubeletReady = "Unknown"`
- `KubeletVersion = "unknown"`
- `LastUpdatedBy = DriftDetectionAndRemediation`
- `LastUpdatedReason = kubernetesVersionDrift`

Cordon/drain and uncordon failures do not mark kubelet unhealthy because they occur before or after the nspawn node replacement and do not necessarily imply kubelet failure.

## E2E Coverage

The default E2E `all` flow includes MSI-node Kubernetes version drift coverage. The suite also exposes an explicit `upgrade-drift` command for running only the drift scenario after infra is deployed.

The flow bootstraps the MSI node with an older Kubernetes minor version, waits for drift remediation to repave to `kube2`, validates kubelet major/minor matches the AKS desired version, verifies the Kubernetes node is Ready, and runs a smoke pod.

Run it after infra is deployed:

```bash
./hack/e2e/run.sh infra
./hack/e2e/run.sh upgrade-drift
```

Optional overrides:

```bash
E2E_DRIFT_INITIAL_KUBERNETES_VERSION=1.34.0 ./hack/e2e/run.sh upgrade-drift
E2E_DRIFT_UPGRADE_TIMEOUT=1200 ./hack/e2e/run.sh upgrade-drift
```

## Current Limitations

- Only Kubernetes major/minor version drift is actionable.
- If failure happens after the old side is stopped but before the new side is fully healthy, rollback is not yet automatic.
- Active side discovery depends on runtime `machinectl` state. If both sides are stopped, current health checks cannot infer the intended recovery target from persisted goal state.

The repave code contains TODOs to refine goal-state persistence and use it for active/desired machine discovery and recovery decisions.
