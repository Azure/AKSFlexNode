# AKS Flex Node

A Go service that extends Azure Kubernetes Service (AKS) to non-Azure VMs through Azure Arc integration, enabling hybrid and edge computing scenarios.

**Status:** Work In Progress
**Platform:** Ubuntu 22.04.5 LTS (tested)
**Architecture:** x86_64 (amd64)

## Overview

AKS Flex Node transforms any Ubuntu VM into a fully managed AKS worker node by:

- 🔗 **Azure Arc Registration** - Registers your VM with Azure Arc for cloud management
- 📦 **Container Runtime Setup** - Installs and configures runc and containerd
- ☸️ **Kubernetes Integration** - Deploys kubelet, kubectl, and kubeadm components
- 🌐 **Network Configuration** - Sets up Container Network Interface (CNI) for pod networking
- 🚀 **Service Orchestration** - Configures and manages all required systemd services
- ⚡ **Cluster Connection** - Securely joins your VM as a worker node to your existing AKS cluster

### Architecture

For a comprehensive understanding of how AKS Flex Node works, see **[ARCHITECTURE.md](ARCHITECTURE.md)**:

- **For Decision Makers & Architects:**
  - System overview and high-level architecture
  - Component interactions and data flow
  - Azure service integration and rationale

- **For Developers & Operators:**
  - Detailed Azure API specifications
  - Authentication and security model
  - Implementation details and code references
  - Complete 11-step bootstrap process

Quick links:
- [System Architecture Diagram](ARCHITECTURE.md#system-architecture) - Visual overview of all components
- [Operational Phases](ARCHITECTURE.md#operational-phases) - Three-phase transformation process
- [Azure Integration](ARCHITECTURE.md#azure-integration) - How Azure services are used
- [Security & Authentication](ARCHITECTURE.md#security--authentication) - Credential management and security
- [Bootstrap Process Details](ARCHITECTURE.md#bootstrap-process-details) - Complete technical walkthrough

## Data Flow

```mermaid
sequenceDiagram
    participant E as 💻 Edge VM
    participant A as 🤖 aks-flex-node
    participant Arc as 🔗 Azure Arc
    participant K as ☸️ AKS Cluster

    Note over E,K: 1. Initial Setup Phase
    E->>A: Start agent
    A->>Arc: Register with Arc (HTTPS)
    Arc-->>A: Managed identity created

    Note over E,K: 2. Bootstrap Phase
    A->>A: Install container runtime
    A->>A: Install Kubernetes components
    A->>A: Configure system settings

    Note over E,K: 3. Cluster Join Phase
    A->>K: Request cluster credentials (HTTPS)
    K-->>A: Provide kubeconfig
    A->>E: Configure kubelet service
    E->>K: Join as worker node (HTTPS/443)

    Note over E,K: 4. Runtime Operations
    K->>E: Schedule workloads
    E->>K: Report node status
    E->>K: Stream logs & metrics
```

## Quick Start

### Prerequisites
- **VM Requirements:**
  - Ubuntu 22.04.5 LTS VM (non-Azure)
  - Minimum 2GB RAM, 25GB free disk space
  - Sudo access on the VM
- **AKS Cluster Requirements:**
  - Azure RBAC enabled AKS cluster
  - Network connectivity from edge VM to cluster API server (port 443)
- **Azure Authentication & Permissions:**
  - The user account or service principal needs the following permissions:
    - **Arc Registration:** `Azure Connected Machine Onboarding` role on the resource group
    - **RBAC Assignment:** `User Access Administrator` or `Owner` role on the AKS cluster to assign roles to the Arc managed identity
    - **AKS Access:** `Azure Kubernetes Service Cluster Admin Role` on the target AKS cluster

### 1. Installation

```bash
# Install aks-flex-node
curl -fsSL https://raw.githubusercontent.com/Azure/AKSFlexNode/main/scripts/install.sh | sudo bash

# Verify installation
aks-flex-node version
```

### 2. Configure
Create the configuration directory and file:

```bash
# Create configuration file
sudo tee /etc/aks-flex-node/config.json > /dev/null << 'EOF'
{
  "azure": {
    "subscriptionId": "your-subscription-id",
    "tenantId": "your-tenant-id",
    "cloud": "AzurePublicCloud",
    "arc": {
      "machineName": "your-unique-node-name",
      "tags": {
        "environment": "edge",
        "node-type": "worker"
      },
      "resourceGroup": "your-resource-group",
      "location": "westus",
      "autoRoleAssignment": true
    },
    "targetCluster": {
      "resourceId": "/subscriptions/your-subscription-id/resourceGroups/your-rg/providers/Microsoft.ContainerService/managedClusters/your-cluster",
      "location": "westus"
    }
  },
  "kubernetes": {
    "version": "your-kubernetes-version"
  },
  "agent": {
    "logLevel": "info",
    "logDir": "/var/log/aks-flex-node"
  }
}
EOF

```

**Important:** Replace the placeholder values with your actual Azure resource information:
- `your-subscription-id`: Your Azure subscription ID
- `your-tenant-id`: Your Azure tenant ID
- `your-unique-node-name`: A unique name for this edge node
- `your-resource-group`: Resource group where Arc machine and AKS cluster are located
- `your-cluster`: Your AKS cluster name


### 3. Usage

#### Available Commands

| Command | Description | Usage |
|---------|-------------|-------|
| `agent` | Start agent daemon (bootstrap + monitoring) | `aks-flex-node agent --config /etc/aks-flex-node/config.json` |
| `unbootstrap` | Clean removal of all components | `aks-flex-node unbootstrap --config /etc/aks-flex-node/config.json` |
| `version` | Show version information | `aks-flex-node version` |

#### Agent Command (Bootstrap + Daemon)
```bash
# Option 1: Direct command execution
aks-flex-node agent --config /etc/aks-flex-node/config.json
cat /var/log/aks-flex-node/aks-flex-node.log

# Option 2: Using systemd service
sudo systemctl enable aks-flex-node-agent.service; sudo systemctl start aks-flex-node-agent
journalctl -u aks-flex-node-agent --since "1 minutes ago" -f

```

#### Unbootstrap
```bash
# Direct command execution
aks-flex-node unbootstrap --config /etc/aks-flex-node/config.json
cat /var/log/aks-flex-node/aks-flex-node.log
```

## Authentication Flow:

### CLI Credential:
When Service Pinciple isn't configured, the service will use az login credential for arc related operations, e.g. joining the VM to Azure as an ARC machine. If you haven't run `az login` or your token is expired, the bootstrap process will automatically prompt you to login interactively
- The login prompt will appear in your terminal with device code authentication when needed
- Once authenticated, the service will use your Azure CLI credentials for operations like arc join and role assignments

### Service Principle:
Alternatively, configure a service principal by adding the following to the config file:
```json
{
  "azure": {
    "subscriptionId": "your-subscription-id",
    "tenantId": "your-tenant-id",
    "servicePrincipal": {
      "clientId": "your-service-principal-client-id",
      "clientSecret": "your-service-principal-client-secret"
    },
    // ... rest of config
  }
}
```

The service principal must have the same permissions listed in the Prerequisites section:
- `Azure Connected Machine Onboarding` role on the resource group
- `User Access Administrator` or `Owner` role on the AKS cluster
- `Azure Kubernetes Service Cluster Admin Role` on the target AKS cluster

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

## Building from Source

For developers who want to build from source:

```bash
# Build the application
make build

# Run tests
make test
```

For a complete list of build targets, run `make help`.

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

### Architecture & Design

- **[Architecture Documentation](ARCHITECTURE.md)** - Comprehensive system architecture
  - High-level architecture diagrams and component interactions
  - Azure API reference with complete specifications
  - Security model and authentication flows
  - Detailed bootstrap process (11 steps)
  - Network requirements and data flow

### Additional Documentation [TO BE ADDED]

- [Development Guide](docs/DEVELOPMENT.md) - Build, test, and contribute
- [Configuration Reference](docs/CONFIGURATION.md) - Complete config options
- [Setup Guide](docs/AKS_EDGE_NODE_SETUP_GUIDE.md) - Step-by-step deployment
- [Troubleshooting](docs/TROUBLESHOOTING.md) - Common issues and solutions
- [APT Packaging](docs/APT_PACKAGING_GUIDE.md) - Package management

## Contributing

1. Fork the repository
2. Create your feature branch (`git checkout -b feature/amazing-feature`)
3. Commit your changes (`git commit -m 'Add amazing feature'`)
4. Push to the branch (`git push origin feature/amazing-feature`)
5. Open a Pull Request

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

## Support

- Report issues: [GitHub Issues](https://github.com/your-org/AKSFlexNode/issues)
- Discussion: [GitHub Discussions](https://github.com/your-org/AKSFlexNode/discussions)
- Email: support@yourorg.com

---

<div align="center">

**🚀 Built with ❤️ for the Kubernetes community**

![Made with Go](https://img.shields.io/badge/Made%20with-Go-00ADD8?style=flat-square&logo=go)
![Kubernetes](https://img.shields.io/badge/Kubernetes-Ready-326CE5?style=flat-square&logo=kubernetes)
![Azure](https://img.shields.io/badge/Azure-Integrated-0078D4?style=flat-square&logo=microsoftazure)

</div>