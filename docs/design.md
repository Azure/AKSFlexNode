# AKS Flex Node - Design Documentation

## Table of Contents

- [Overview](#overview)
- [High-Level Design](#high-level-design)
- [System Components](#system-components)
- [Data Flow & Lifecycle](#data-flow--lifecycle)
- [Azure Integration](#azure-integration)
- [Security & Authentication](#security--authentication)
- [References](#references)

---

## Overview

**Key Technologies:**
- **Language:** Go 1.24+
- **Target Platform:** Ubuntu 22.04 LTS or 24.04 LTS
- **Architecture:** x86_64 (amd64) or arm64
- **Container Runtime:** containerd + runc
- **Kubernetes Components:** kubelet, kubectl, kubeadm
- **Azure Integration:** Azure RBAC, Optional Azure Arc with Managed Identity


### Deployment Modes

AKS Flex Node supports two deployment modes:

1. **With Azure Arc**
   - VM registered as Azure Arc-enabled server
   - Kubelet uses Arc-managed identity for authentication
   - Enhanced cloud visibility and management
   - Automatic credential rotation

2. **Without Azure Arc**
   - No Arc registration required
   - Kubelet uses Service Principal for authentication
   - Simplified deployment for environments where Arc is not desired
   - Manual credential management

---

## High-Level Design

### System Overview

![System Overview](../diagrams/system-overview.svg)

**How to Read:**

- **Layers** (spatial - WHERE):
  - 👤 Blue = User Layer
  - ☁️ Yellow = Azure Cloud Services
  - 💻 Green = VM/Node
  - 📦 Purple = Workloads

- **Phases** (temporal - WHEN):
  - **⟹ Steps 1-4**: Identity Setup
  - **- - → Steps 5-9**: Installation
  - **→ Steps 10-12**: Activation
  - **···· Unlabeled**: Runtime (continuous)

### Operational Phases

![Operational Phases](../diagrams/operational-phases.svg)

**Phase 1: Identity Setup**
- Authenticate user credentials
- **(Optional)** Register VM with Azure Arc (creates managed identity)
- **(Optional)** Assign RBAC permissions to the Arc identity
- **(Non-Arc mode)** Validate Service Principal credentials

**Phase 2: Installation**
- Configure system (kernel settings, directories)
- Install container runtime (containerd + runc)
- Install Kubernetes components (kubelet, kubectl, kubeadm)
- Setup CNI networking plugins

**Phase 3: Activation**
- Download cluster configuration from AKS
- Configure kubelet authentication:
  - **With Arc:** Use Arc-managed identity
  - **Without Arc:** Use Service Principal credentials
- Start services (containerd, kubelet)
- Node joins cluster automatically

---

## System Components

### Component Responsibilities

| Component | What It Does | When It Runs |
|-----------|--------------|--------------|
| **AKS Flex Node Agent** | Orchestrates VM transformation | Bootstrap phase only |
| **Azure Arc Agent** (Optional) | Manages VM identity and authentication | Bootstrap + Runtime (Arc mode only) |
| **Container Runtime** | Executes containerized applications | Runtime phase |
| **Kubelet** | Communicates with AKS control plane | Runtime phase |
| **CNI Plugins** | Enables pod-to-pod networking | Runtime phase |

### Component Interactions

![Component Interactions](../diagrams/component-interactions.svg)

**Bootstrap Sequence (①-⑧):** One-time setup
- User initiates transformation
- **(Optional)** Agent registers with Arc (creates identity)
- **(Optional)** Agent assigns RBAC permissions
- Agent downloads cluster configuration
- Agent installs runtime components

**Runtime Operations (⑨-⑬):** Ongoing interactions
- **(Arc mode)** Arc Agent provides identity tokens to Kubelet
- **(Non-Arc mode)** Kubelet uses Service Principal for authentication
- Kubelet authenticates and registers with cluster
- Kubelet manages container lifecycle
- Containers use CNI for networking

---

## Data Flow & Lifecycle

### Bootstrap Workflow

![Bootstrap Workflow](../diagrams/bootstrap-workflow.svg)

### Phase Breakdown

**Phase 1: Identity Setup** (1-5 minutes with Arc, <1 minute without)
- **Purpose**: Establish trust between VM and AKS cluster
- **With Arc**: VM registered with Arc, managed identity created, RBAC permissions assigned
- **Without Arc**: Service Principal credentials validated
- **Outcome**: Authentication configured for cluster access

**Phase 2: Installation** (5-10 minutes)
- **Purpose**: Prepare VM to run Kubernetes workloads
- **Outcome**: All required software installed and configured

**Phase 3: Activation** (1-2 minutes)
- **Purpose**: Connect VM to AKS cluster
- **With Arc**: Kubelet configured to use Arc-managed identity
- **Without Arc**: Kubelet configured to use Service Principal
- **Outcome**: Node is running and accepting workload assignments

**Runtime Operation** (Continuous)
- **Purpose**: Execute workloads assigned by cluster
- **Duration**: Until node is decommissioned

---

## Azure Integration

### Azure APIs Used

The agent calls these Azure APIs during bootstrap:

| Azure Service | API Purpose | Required | Azure API Documentation |
|---------------|-------------|----------|------------------------|
| **Azure Container Service** | Download cluster credentials | Always | [AKS API](https://learn.microsoft.com/rest/api/aks/) |
| **Azure AD** | Authenticate for API calls | Always | [Azure Identity](https://learn.microsoft.com/azure/developer/go/azure-sdk-authentication) |
| **Azure Arc** | Register VM, get managed identity | Arc mode only | [Hybrid Compute API](https://learn.microsoft.com/rest/api/hybridcompute/) |
| **Azure RBAC** | Assign cluster permissions | Arc mode only | [Authorization API](https://learn.microsoft.com/rest/api/authorization/) |

### Bootstrap Flow by Mode

**With Azure Arc enabled:**
1. Authenticates to Azure AD (Service Principal or Azure CLI)
2. Registers VM with Azure Arc → creates managed identity
3. Assigns RBAC roles to the managed identity
4. Downloads kubeconfig from AKS API
5. Configures kubelet to use Arc managed identity

**Without Azure Arc:**
1. Validates Service Principal credentials
2. Downloads kubeconfig from AKS API
3. Configures kubelet to use Service Principal for authentication

---

## Security & Authentication

### Authentication Flow

#### With Azure Arc Enabled

**Bootstrap Phase:**
- Uses Service Principal OR Azure CLI credentials
- Authenticates to Azure AD
- Used for Arc registration, RBAC assignment, kubeconfig download

**Runtime Phase:**
- Kubelet uses Arc managed identity (HIMDS)
- Token script at `/var/lib/kubelet/token.sh`
- Auto-rotated, short-lived tokens
- No manual credential management needed

#### Without Azure Arc

**Bootstrap Phase:**
- Uses Service Principal credentials (required)
- Authenticates to Azure AD
- Used for kubeconfig download

**Runtime Phase:**
- Kubelet uses Service Principal for authentication
- Static credentials stored in kubeconfig
- Manual credential rotation required

### Required Permissions

#### For Arc Mode

**User/Service Principal (Bootstrap):**
- `Azure Connected Machine Onboarding` - Register with Arc
- `User Access Administrator` or `Owner` - Assign RBAC roles
- `Azure Kubernetes Service Cluster Admin Role` - Download credentials

**Arc Managed Identity (Runtime):**
- `Azure Kubernetes Service Cluster User Role` - Assigned by agent during bootstrap

#### For Non-Arc Mode

**Service Principal (Bootstrap + Runtime):**
- `Azure Kubernetes Service Cluster Admin Role` - Download credentials (bootstrap)
- `Azure Kubernetes Service Cluster User Role` - Kubelet authentication (runtime)

**Azure Docs:** [Azure RBAC Built-in Roles](https://learn.microsoft.com/azure/role-based-access-control/built-in-roles)

---

## References

### Azure Documentation
- [Azure Arc-enabled servers](https://learn.microsoft.com/azure/azure-arc/servers/overview)
- [Azure Arc managed identity](https://learn.microsoft.com/azure/azure-arc/servers/managed-identity-authentication)
- [Azure RBAC](https://learn.microsoft.com/azure/role-based-access-control/overview)
- [AKS REST API](https://learn.microsoft.com/rest/api/aks/)

### Kubernetes Documentation
- [Kubelet Configuration](https://kubernetes.io/docs/reference/command-line-tools-reference/kubelet/)
- [Exec Credential Plugin](https://kubernetes.io/docs/reference/access-authn-authz/authentication/#client-go-credential-plugins)
- [Node API](https://kubernetes.io/docs/reference/kubernetes-api/cluster-resources/node-v1/)

### Container Runtime Documentation
- [containerd](https://containerd.io/)
- [runc](https://github.com/opencontainers/runc)
- [CNI Specification](https://github.com/containernetworking/cni)

### Code Repository
- [AKS Flex Node Source Code](https://github.com/Azure/AKSFlexNode)
- [Azure SDK for Go](https://github.com/Azure/azure-sdk-for-go)

---

**Version:** 1.1
**Last Updated:** 2026-02-03
**Feedback:** [GitHub Issues](https://github.com/Azure/AKSFlexNode/issues)
