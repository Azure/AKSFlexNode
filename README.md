# AKS Flex Node

## Overview

AKS Flex Node extends Azure Kubernetes Service (AKS) to customer-managed virtual machines and bare metal hosts, enabling them to run as AKS worker nodes outside standard AKS node pools. It is built on top of [Azure Unbounded](https://github.com/Azure/unbounded), which provides the host-side foundation for running and reconciling isolated Kubernetes node environments.

> **Status:** AKS Flex Node is currently alpha software.

## Key Features And Scenarios

- Bootstrap and join virtual machines or bare metal hosts for both amd64 and arm64 as AKS worker nodes.
- Support hybrid, lab, and specialized hardware scenarios.
- Use flexible authentication modes, including Azure Arc, managed identity (MSI), and Kubernetes bootstrap token.
- Automatically detect NVIDIA GPU devices and configure the container runtime for accelerated workloads.
- Run blue-green in-place updates and upgrades while retaining the existing host.
- Manage your Flex Node fleet through AKS management APIs for upgrade, repair, reset, and related lifecycle operations.
- Remediate and repair agent and node state through first-class lifecycle operations.

## Getting Started

Install the latest `aks-flex-node` binary with the install script:

```bash
sudo su
curl -fsSL https://raw.githubusercontent.com/Azure/AKSFlexNode/main/scripts/install.sh | bash
aks-flex-node version
```

After installation, create a configuration file for your authentication mode and target AKS cluster, then bootstrap the node:

```bash
aks-flex-node bootstrap --config /etc/aks-flex-node/config.json
```

`bootstrap` installs host components, starts the local AKS worker node, installs the systemd unit, and starts the agent daemon.

## Usage Guides And Topics

- [Usage Guide](docs/usage.md) - Installation, configuration, authentication modes, operations, and troubleshooting.
- [Design Documentation](docs/design.md) - Architecture, lifecycle, Azure integration, and security model.

## Development And Security

- [Development Guide](docs/development.md) - To learn more about build, development, and contribution workflow.
- [Security Policy](SECURITY.md) - How to report security vulnerabilities.

## License

This project is licensed under the MIT License. See [LICENSE](LICENSE.MD) for details.

---

<div align="center">

**🚀 Built with ❤️ for the Kubernetes community**

![Made with Go](https://img.shields.io/badge/Made%20with-Go-00ADD8?style=flat-square&logo=go)
![Kubernetes](https://img.shields.io/badge/Kubernetes-Ready-326CE5?style=flat-square&logo=kubernetes)
![Azure](https://img.shields.io/badge/Azure-Integrated-0078D4?style=flat-square&logo=microsoftazure)

</div>
