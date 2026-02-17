# Components API Design

## Motivation

Node bootstrapping involves many steps -- installing packages, downloading binaries, writing config files, starting services, joining a cluster. These steps vary across OS distros, Kubernetes versions, and runtime configurations. Without a structured API, the logic becomes a tangle of procedural scripts that are hard to version, hard to compose, and hard to make reliable.

The Components API addresses this by introducing a versioned, declarative action model.

## Design Goals

### Versioned Specs for Compatibility

Every action is defined as a Protocol Buffers message with a date-based version (e.g. `v20260301`). The spec version travels with the request, so the implementation can inspect it and behave accordingly:

- A newer binary can still handle older spec versions by maintaining backward-compatible handlers.
- An older binary can reject a spec version it does not understand, rather than silently doing the wrong thing.

This makes rolling upgrades safe. The control plane and the node agent do not need to be updated in lockstep -- as long as the spec version is understood, the action will be applied correctly.

```protobuf
// Each action carries Metadata (including type), a versioned Spec, and a Status.
message DownloadCRIBinaries {
  api.Metadata metadata = 1;
  DownloadCRIBinariesSpec spec = 2;
  DownloadCRIBinariesStatus status = 3;
}
```

### Declarative Actions over Procedures

Actions describe **what** should be true, not **how** to make it true. The caller says "containerd 2.0.1 should be installed" rather than scripting the download, extraction, and permission steps.

This has several benefits:

- **Distro abstraction** -- the implementation behind an action can differ between Ubuntu, AzureLinux, or any other distro. The caller does not need to know which distro is running; it submits the same spec regardless.
- **Idempotency** -- because the action describes desired state, the implementation can check whether the state is already met and skip redundant work. Applying the same action twice is safe.
- **Internal retry** -- the implementation can retry transient failures (network errors, lock contention) without the caller needing retry logic. The declarative boundary gives the implementation freedom to choose its own strategy.

### Composable Node Flavors

Actions are independent, self-contained units. A node configuration is simply a list of actions applied in sequence:

```json
[
  { "metadata": { "type": "...ConfigureBaseOS" },      "spec": { ... } },
  { "metadata": { "type": "...DownloadCRIBinaries" },  "spec": { ... } },
  { "metadata": { "type": "...StartContainerdService" }, "spec": { ... } },
  { "metadata": { "type": "...DownloadKubeBinaries" }, "spec": { ... } },
  { "metadata": { "type": "...KubadmNodeJoin" },       "spec": { ... } }
]
```

Different combinations produce different node flavors:

- A GPU node might include additional actions for GPU driver and device plugin setup.
- A confidential computing node might add attestation and encryption actions.
- A minimal node might omit optional components entirely.

The control plane composes the action list for each node based on its desired role and capabilities. No code changes are needed to create a new flavor -- just a different list of actions with different specs.

## Action Structure

Every action follows a consistent three-part structure:

```protobuf
message <ActionName> {
  api.Metadata metadata = 1;       // Identifies the action type and name
  <ActionName>Spec spec = 2;       // Desired state (input from caller)
  <ActionName>Status status = 3;   // Observed state (output from handler)
}
```

| Field      | Role                                                             |
|------------|------------------------------------------------------------------|
| `metadata` | Carries the action type URL and an optional human-readable name. |
| `spec`     | Caller-provided desired state. May include defaulting and validation. |
| `status`   | Handler-populated observed state, returned after the action completes. |

Spec types can optionally implement `Defaulting()` and `Validate()` to fill in omitted fields with sensible defaults and reject invalid input before execution begins.

## Example Configurations

The node lifecycle involves distinct phases -- image baking, bootstrapping, upgrading, re-imaging, and more (see [Agent Node Host Environment](#agent-node-host-environment) below). Each phase requires a different subset of actions. The Components API supports this by letting the caller supply a different action list per phase.

### Image Baking

During image baking, the goal is to pre-install software so that bootstrapping is fast. Cluster-specific configuration and credentials are deliberately left out.

```json
[
  { "metadata": { "type": "...ConfigureBaseOS" },     "spec": {} },
  { "metadata": { "type": "...DownloadCRIBinaries" }, "spec": { "containerdVersion": "2.0.1", "runcVersion": "1.2.4" } },
  { "metadata": { "type": "...DownloadKubeBinaries" }, "spec": { "kubernetesVersion": "1.32.1" } }
]
```

### Node Bootstrapping

At bootstrap time, the actions pick up where the baked image left off -- starting services and joining the cluster.

```json
[
  { "metadata": { "type": "...StartContainerdService" }, "spec": { "sandboxImage": "mcr.microsoft.com/oss/containerd/pause:3.6" } },
  { "metadata": { "type": "...KubadmNodeJoin" },       "spec": { "controlPlane": { "server": "https://aks-cluster-cp.hcp.eastus2.azmk8s.io:443", "certificateAuthorityData": "..." }, "kubelet": { "bootstrapAuthInfo": { "token": "..." } } } }
]
```

### Bootstrapping without a Pre-baked Image

In environments where pre-built VHD images are not available, the action list includes the download steps alongside the bootstrap steps. Because actions are idempotent, this same list also works safely on a baked image -- the download actions detect that binaries are already present and skip redundant work.

```json
[
  { "metadata": { "type": "...ConfigureBaseOS" },      "spec": {} },
  { "metadata": { "type": "...DownloadCRIBinaries" },  "spec": { "containerdVersion": "2.0.1", "runcVersion": "1.2.4" } },
  { "metadata": { "type": "...StartContainerdService" }, "spec": { "sandboxImage": "mcr.microsoft.com/oss/containerd/pause:3.6" } },
  { "metadata": { "type": "...DownloadKubeBinaries" }, "spec": { "kubernetesVersion": "1.32.1" } },
  { "metadata": { "type": "...KubadmNodeJoin" },       "spec": { "controlPlane": { "server": "https://cp.example.com:6443", "certificateAuthorityData": "..." }, "kubelet": { "bootstrapAuthInfo": { "token": "..." } } } }
]
```

### Node Component Upgrades

An in-place upgrade supplies only the actions for the components being updated. For example, upgrading the Kubernetes version:

```json
[
  { "metadata": { "type": "...DownloadKubeBinaries" }, "spec": { "kubernetesVersion": "1.33.0" } }
]
```

The action detects the version mismatch and replaces the binaries. Other components are untouched.

