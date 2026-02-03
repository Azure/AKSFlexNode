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

For detailed setup instructions, prerequisites, requirements, and configuration options, see the **[Usage Guide](docs/usage.md)**.

## Contributing

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