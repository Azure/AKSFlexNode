# AKS Flex Node

A Go agent that extends Azure Kubernetes Service (AKS) to non-Azure VMs, enabling hybrid and edge computing scenarios. Optionally integrates with Azure Arc for enhanced cloud management capabilities.

**Status:** Work In Progress
**Platform:** Ubuntu 22.04 LTS, Ubuntu 24.04 LTS
**Architecture:** x86_64 (amd64), arm64

## Overview

AKS Flex Node transforms any Ubuntu VM into a semi-managed AKS worker node by:

- 📦 **Container Runtime Setup** - Installs and configures runc and containerd
- ☸️ **Kubernetes Integration** - Deploys kubelet, kubectl, and kubeadm components
- 🌐 **Network Configuration** - Sets up Container Network Interface (CNI) for pod networking
- 🚀 **Service Orchestration** - Configures and manages all required systemd services
- ⚡ **Cluster Connection** - Securely joins your VM as a worker node to your existing AKS cluster
- 🔗 **Azure Arc Registration** (Optional) - Registers your VM with Azure Arc for cloud management and managed identity

## Documentation

- **[Usage Guide](docs/usage.md)** - Installation, configuration, and usage instructions
- **[Design Documentation](docs/design.md)** - System design, data flow, Azure integration, and technical specifications
- **[Development Guide](docs/development.md)** - Building from source, testing, and contributing

## Quick Start

### Installation

```bash
# Install aks-flex-node
curl -fsSL https://raw.githubusercontent.com/Azure/AKSFlexNode/main/scripts/install.sh | sudo bash

# Verify installation
aks-flex-node version
```

### Usage

```bash
# Start the agent
aks-flex-node agent --config /etc/aks-flex-node/config.json
```

<<<<<<< HEAD
For detailed setup instructions, prerequisites, requirements, and configuration options, see the **[Usage Guide](docs/usage.md)**.
=======
After you've set the correct config and started the agent, it takes a while to finish all the steps. If you used systemd service, as mentioned above, you can use

```bash
journalctl -u aks-flex-node-agent --since "1 minutes ago" -f
```

to view logs and see if anything goes wrong. If everything works fine, after a while, you would see the following:

- In the resource group you specified in the config file, you should see a new resource added by Azure Arc with type Microsoft.HybridCompute/machines
- Running "kubectl get nodes" against your cluster should see the new node added and in "Ready" state

#### Unbootstrap
```bash
# Direct command execution
aks-flex-node unbootstrap --config /etc/aks-flex-node/config.json
cat /var/log/aks-flex-node/aks-flex-node.log
```

## Authentication Flow:

AKS Flex Node supports three authentication methods (in order of precedence):

### 1. Azure Arc (Managed Identity)
When Arc is enabled, the Arc-registered machine's system-assigned managed identity is used. This is automatically configured during Arc registration.

### 2. VM Managed Identity (MSI)
For Azure VMs with managed identities, configure MSI authentication:
```json
{
  "azure": {
    "subscriptionId": "your-subscription-id",
    "tenantId": "your-tenant-id",
    "managedIdentity": {},
    // ... rest of config
  }
}
```

For VMs with multiple user-assigned managed identities, specify the ClientID:
```json
{
  "azure": {
    "subscriptionId": "your-subscription-id",
    "tenantId": "your-tenant-id",
    "managedIdentity": {
      "clientId": "your-managed-identity-client-id"
    },
    // ... rest of config
  }
}
```

The VM's managed identity must have:
- `Azure Kubernetes Service Cluster Admin Role` on the target AKS cluster
- `Azure Kubernetes Service RBAC Cluster Admin` on the target AKS cluster

### 3. Service Principal
Configure a service principal by adding the following to the config file:
```json
{
  "azure": {
    "subscriptionId": "your-subscription-id",
    "tenantId": "your-tenant-id",
    "servicePrincipal": {
      "tenantId": "your-tenant-id",
      "clientId": "your-service-principal-client-id",
      "clientSecret": "your-service-principal-client-secret"
    },
    // ... rest of config
  }
}
```

The service principal must have:
- `Azure Connected Machine Onboarding` role on the resource group (for Arc registration)
- `User Access Administrator` or `Owner` role on the AKS cluster (for role assignments)
- `Azure Kubernetes Service Cluster Admin Role` on the target AKS cluster

### 4. Azure CLI Credential (Fallback)
If none of the above are configured, the service will use `az login` credentials for Arc-related operations (e.g., joining the VM to Azure as an Arc machine).
- If you haven't run `az login` or your token is expired, the bootstrap process will automatically prompt you to login interactively
- The login prompt will appear in your terminal with device code authentication when needed
- Once authenticated, the service will use your Azure CLI credentials for operations like Arc registration and role assignments

## Uninstallation

### Complete Removal
```bash
# First run unbootstrap to cleanly disconnect from Arc and AKS cluster
aks-flex-node unbootstrap --config /etc/aks-flex-node/config.json

# Then run automated uninstall to remove all components
curl -fsSL https://raw.githubusercontent.com/Azure/AKSFlexNode/main/scripts/uninstall.sh | sudo bash
```

The uninstall script will:
- Stop and disable aks-flex-node agent service
- Remove the service user and permissions
- Clean up all directories and configuration files
- Remove the binary and systemd service files

### Force Uninstall (Non-interactive)
```bash
# For automated environments where confirmation prompts should be skipped
curl -fsSL https://raw.githubusercontent.com/Azure/AKSFlexNode/main/scripts/uninstall.sh | sudo bash -s -- --force
```

**⚠️ Important Notes:**
- Run `aks-flex-node unbootstrap` first to properly disconnect from Arc and clean up Azure resources
- The uninstall script will NOT disconnect from Arc - this ensures proper cleanup order
- The Azure Arc agent remains installed but can be removed manually if not needed
- Backup any important data before uninstalling

## Contributing

Interested in contributing to AKS Flex Node? See [CONTRIBUTING.md](CONTRIBUTING.md) for:

- Development setup and building from source
- Running unit tests and E2E tests
- Setting up E2E testing infrastructure
- Code quality checks and linting
- Pull request process

Quick start for contributors:

```bash
# Build the application
make build

# Run tests
make test

# Run all quality checks
make check
```

## System Requirements

- **Operating System:** Ubuntu 22.04.5 LTS
- **Architecture:** x86_64 (amd64)
- **Memory:** Minimum 2GB RAM (4GB recommended)
- **Storage:**
  - **Minimum:** 25GB free space
  - **Recommended:** 40GB free space
  - **Production:** 50GB+ free space
- **Network:** Internet connectivity to Azure endpoints
- **Privileges:** Root/sudo access required
- **Build Dependencies:** Go 1.24+ (if building from source)

### Storage Breakdown
- **Base components:** ~3GB (Arc agent, runc, containerd, Kubernetes binaries, CNI plugins)
- **System directories:** ~5-10GB (`/var/lib/containerd`, `/var/lib/kubelet`, configurations)
- **Container images:** ~5-15GB (pause container, system images, workload images)
- **Logs:** ~2-5GB (`/var/log/pods`, `/var/log/containers`, agent logs)
- **Installation buffer:** ~5-10GB (temporary downloads, garbage collection headroom)


## Documentation

### For Users

- **[README.md](README.md)** - Getting started, installation, and usage (this document)
- **[Architecture Documentation](ARCHITECTURE.md)** - Comprehensive system architecture
  - High-level architecture diagrams and component interactions
  - Azure API reference with complete specifications
  - Security model and authentication flows
  - Detailed bootstrap process (11 steps)
  - Network requirements and data flow

### For Contributors

- **[CONTRIBUTING.md](CONTRIBUTING.md)** - Development guide
  - Building from source and development setup
  - Running tests (unit tests and E2E tests)
  - Setting up E2E testing infrastructure
  - Code quality checks and linting
  - Pull request process
>>>>>>> 1eaeb32 (Update workflow and documentation for explicit MSI configuration)

### Advanced Topics

We welcome contributions! See the **[Development Guide](docs/development.md)** for details on building, testing, and submitting pull requests.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE.MD) file for details.

## Security

Microsoft takes the security of our software products and services seriously. If you believe you have found a security vulnerability, please report it to us as described in [SECURITY.md](SECURITY.md).

---

<div align="center">

**🚀 Built with ❤️ for the Kubernetes community**

![Made with Go](https://img.shields.io/badge/Made%20with-Go-00ADD8?style=flat-square&logo=go)
![Kubernetes](https://img.shields.io/badge/Kubernetes-Ready-326CE5?style=flat-square&logo=kubernetes)
![Azure](https://img.shields.io/badge/Azure-Integrated-0078D4?style=flat-square&logo=microsoftazure)

</div>
