# Agent Node Host Environment

## Overview

Each Kubernetes agent (worker) node must be provisioned with the software and services
required to join a cluster and run workloads. Beyond this minimal baseline, certain
scenarios demand additional setup; for example, GPU-capable nodes may need NVIDIA
drivers and the appropriate device plugin to expose GPU resources to Kubernetes.

At the same time, agent nodes are routinely restarted, patched, or replaced as part of
ongoing maintenance and upgrade cycles. The mechanisms for performing these lifecycle
operations vary across cloud and on-prem environments, depending on the available APIs
and underlying infrastructure.

To ensure AKS flex nodes can function consistently across this broad range of environments,
this document defines the baseline runtime assumptions and requirements, and describes
the expected behaviors for key lifecycle operations.

### Non-Goals

- We will limit the support scope to Linux-based nodes and focus on Ubuntu distro for now.
  This is because Ubuntu is the widely and commonly available Linux distribution
  across the target environments.
- Credential management (bootstrap token distribution & rotation, CA renewal, etc)
  is out of scope for this document, but will be handled by the operations described below.
- Extra security harding and compliance requirements are out of scope for this document,
  but can be added as optional layers on top of the baseline environment in the future.
- Detailed GPU device plugin requirements and enablement strategies will be addressed in
  a separate document.

## Baseline Environment Requirements

### CPU Only Nodes

- A Linux-based OS with `systemd` init system;
- Modern Linux kernel (currently LTS or supported release, minimum 5.19) enabled
  with cgroup v2, namespaces, overlayfs, eBPF etc for container support.
- Swap disabled;
- System level logging enabled with rotation configured;
- Time synchronization configured;
- Proper host level DNS setup;
- Outbound connectivity to cluster control plane endpoint;
- Container runtime components:
  * `containerd` w/ 2.0+ version;
  * `runc`
- Kubernetes components:
  * `kubelet` matching with the target worker node version;
  * Other cloud provider binaries;
- NFTables / IPtables installed for Kubernetes network policies;
- Network forward, IP masquerade and bridge settings configured for Kubernetes networking;
- Support tools / binaries (e.g., `curl`, `ping`, etc) for diagnostics and troubleshooting;
- Configurations:
  * Standard container runtime configurations layout on the host;
  * Standard Kubernetes node configurations layout on the host;
  * Control plane public CA certificate(s);
  * TLS bootstrap configurations;

### GPU-Capable Nodes

- All of the above CPU node requirements;
- GPU drivers and runtime (e.g. NVIDIA drivers and CUDA toolkit for NVIDIA GPUs)
  compatible with OS kernel;
- RDMA, SR-IOV and InfiniBand drivers and runtime for GPU direct communication (if applicable);
- Configurations:
  * Updated container runtime configurations with support for GPU drivers and runtimes;

### Additional Requirements

- Node identity for identifying and authenticating the node to cluster control plane;
- CNI plugin binaries and configurations;
- Node-problem-detector;
- Node local DNS caching;
- VPN components for cross region/cloud connectivity;
- Background auto-repair agent;
- Pre-cached container images for critical system components;
- Support for adding optional feature layers & customizations during node image
  baking or bootstrapping process;
- In some environments, pre-built VHD images might not be available. In such
  cases, the node bootstrapping process should also handle the initial OS image
  baking and provisioning to ensure a consistent baseline environment.
- In some environments, the node might have limited outbound connectivity
  (e.g., no direct access to public internet). In such cases, the node bootstrapping
  process should also handle pulling necessary components through proxy or fallback endpoints.

## Node Lifecycle Operations

This section describes lifecycle activities across heterogeneous environments.
Each operation defines:

- **Inputs**: what information/config is required
- **Actions**: what the platform does
- **Expected behaviors**: node and cluster-level outcomes
- **Failure handling**: what happens when things go wrong

### Node VHD Image Baking

**Purpose**: Produce a base node image (VHD or similar) that satisfies baseline
requirements and can be instantiated consistently across environments.

**Inputs**:

- Base OS distribution and version (e.g., Ubuntu 24.04)
- System configurations
- Versions of container runtime, kubelet, and other components
- Optional feature layers (e.g., GPU drivers)

**Actions**:

- Install and configure system settings & tunings
- Install container runtime and Kubernetes components
- **Leave out**: cluster-specific configurations / credentials

**Expected behaviors**:

- Produced image is **immutable**[^1] and **reproducible**[^2] giving the same inputs.
- Sources for all installed components **MUST** be pinned with qualified versions
  and checksums for traceability and security.
- Every baking step fully completes without partial failures.
- Image is able to boot successfully and reach a "ready-to-bootstrap" state.
- GPU image boot with drivers loaded.

[^1]: Immutable means once the image is built and published, it should not be modified.
      Any updates or changes should trigger a new image build with a new version/tag.

[^2]: Reproducible means given the same inputs and build process, the output image
      should be identical with the installed components/configurations setup.

**Failure handling**:

- Build pipeline produces actionable error messages
- Failed builds do not produce or overwrite existing images

### Node Bootstrapping

**Purpose**: Turn a newly created machine instance into a functional Kubernetes
node that can join the cluster and serve workloads.

**Inputs**:

- Cluster endpoint (API server URL, CA bundle)
- Kubelet bootstrap credentials (node identity credentials)
- Node configuration (e.g., kubelet config, runtime settings, node labels/taints)
- Environment-specific instance metadata (node name, region/zone). Can be
  exposed later via cloud provider.
- VPN configurations for cross region/cloud connectivity if applicable

**Actions**:

- Ensure network & container runtime are ready
- Render kubelet configuration and start kubelet
- Kubelet performs TLS bootstrapping to obtain node credentials and join the
  cluster
- Deploy and enable per node workloads

**Expected behaviors**:

- Node becomes `Ready` within a target SLA
- Node labels/taints are applied correctly
- Node reports correct capacity/allocatable resources, including GPU
  if applicable
- Bootstrap process is **idempotent** and can be safely re-run on the same node
  for transient failures

**Failure handling**:

- Ability to detect and report failure details and kind of failure
  (i.e., transient vs terminal) for better troubleshooting and remediation

### Node Bootstrapping w/ Baking

**Purpose**: In environments without pre-baked images, the bootstrapping process
should also handle the initial image baking and provisioning to ensure a
consistent baseline environment.

**Inputs**:

- Same as Node VHD Image Baking and Node Bootstrapping
- (Optional) fallback/alternative endpoints for pulling necessary components in
  environments with limited outbound connectivity

**Actions**:

- Perform image baking steps as described in Node VHD Image Baking
- Proceed with bootstrapping steps as described in Node Bootstrapping

**Expected behaviors**:

- All expected behaviors from both Node VHD Image Baking and Node Bootstrapping
- In addition, the process should be resilient to transient failures both phases
  and support **idempotent** retries.

**Failure handling**:

- All failure handling mechanisms from both Node VHD Image Baking and Node Bootstrapping

### Node Rebooting & Repairing

_TODO_: This part needs more work and discussions

**Purpose**: Handle planned and unplanned node reboots and repairs while
maintaining node health and minimizing disruption to workloads.

**Node Rebooting**

- Inputs: node name and reboot type (planned vs unplanned)
- Expected behaviors:
  * Node is cordoned/drained before planned reboot
  * Node becomes `Ready` within a target SLA after reboot

**Node Repairing**

- Inputs: node name and repair category
- Expected behaviors:
  * Monitoring components detect node issues and trigger repair actions
  * Impacted services are being restarted

**Failure handling**:

- If node fails to recover within a defined SLA, it should be marked as
  unhealthy and trigger replacement workflow.
- In case of repair failures, exponential backoff retries should be attempted;
  errors should be exposed for troubleshooting and alerting.

### Node Components Version Upgrades

_TODO_: This part needs more details and breakdown designs

**Purpose**: Upgrade on node components (kubelet, container runtime,
CNI plugins) to newer versions.

**Inputs**:

- Target versions for components
- Upgrade strategy (e.g., in-place vs replacement)

**Actions**:

- Cordon/drain node to evict workloads
- In-place upgrade:
  * Install newer versions of components/configurations
  * Restart necessary services
  * Verify node health and functionality
  * Uncordon node
- Replacement upgrade:
  * Deprovision existing node and underlying resources
  * Provision new node with updated image or configurations
  * Join new node to cluster and verify health

**Expected behaviors**:

- Node is reporting expected versions for components after upgrade
- In-place upgrade process is idempotent and can be safely retried in case of
  transient failures

**Failure handling**:

- Failures should be reported for troubleshooting and alerting
- In-place upgrade failures should not leave node open to scheduling.
  Provide rollback if possible or recommend node replacement otherwise.

### Node Re-imaging

**Purpose**: Re-image a node to restore it to a known good state, either for
recovering from failures or applying updates.

**Inputs**:

- Same as Node Bootstrapping, plus:
- Target node and target node image

**Actions**:

- Cordon and drain the node to evict workloads
- Re-image the underlying machine instance with the target image
- Perform bootstrapping steps to rejoin the cluster

**Expected behaviors**:

- Re-image results in a clean, baseline-compliant host state.
- Node returns to `Ready` state within a target SLA.
- Node identity (e.g., name) is preserved after re-imaging.
- Re-image is idempotent and can be safely retried in case of transient failures.

**Failure handling**:

- Re-image failures should be reported for troubleshooting and alerting
- Re-image failed node should not be left in schedulable state.

### Node Deletion

**Purpose**: Remove a node from the cluster intentionally, either for
scaling down, decommissioning, or drift replacement.

**Inputs**: node name

**Actions**:

- Cordon and drain the node to evict workloads if not forced deletion
- Delete node object from cluster
- Deprovision underlying compute / network resources if applicable

**Expected behaviors**:

- Node is gracefully removed from cluster and workloads are rescheduled
- No orphaned resources are left behind

**Failure handling**:

- PDB violations or other issues preventing eviction should be reported clearly
- Infrastructure resource clean up failure should be retried and alerted
  if not successful after SLA