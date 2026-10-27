# Changelog

All notable user-facing changes to AKS Flex Node are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added

- Support separate embedded kubeconfigs for host-agent and kubelet Kubernetes authentication ([#312](https://github.com/Azure/AKSFlexNode/pull/312)).

- Support for Azure Container Linux and other hosts with a read-only `/usr`, and a Butane config, `scripts/aks-flex-node-bootstrap.bu`, that bootstraps a host provisioned by Ignition ([#323](https://github.com/Azure/AKSFlexNode/pull/323)).

### Changed

- **Breaking change:** The agent's binaries and helpers move to `/opt/unbounded/agent`, and `aks-flex-node` is no longer on the default `PATH`; run `/opt/unbounded/agent/bin/aks-flex-node`. Hosts installed by an earlier release move there on their own after AgentUpgrades. `/opt` must allow running programs. `bootstrap.sh --install-dir` and `AKS_FLEX_NODE_INSTALL_DIR` are deprecated and ignored ([#323](https://github.com/Azure/AKSFlexNode/pull/323)).
- **Breaking change:** `AgentUpgrade` MachineOperations now require `parameters.sha256`, the SHA-256 of the compressed release archive. Requests with missing, blank, or malformed digests fail with `InvalidParameters`; mismatched digests fail before extraction or candidate execution. Update upgrade controllers and scripts to supply the digest for each target version and architecture from the release's `checksums.txt` before deploying agents with this requirement.

### Fixed

- Uninstall the agent service only after disabling it, removing its unit files, and reloading systemd, preventing self-uninstall from leaving stale systemd state ([#371](https://github.com/Azure/AKSFlexNode/pull/371)).
- Re-running `bootstrap.sh` on a host that was reset now installs the downloaded agent instead of leaving the service on the previous binary. Like `install.sh`, it removes the binary layout reset keeps, and refuses while the agent service is installed or active ([#376](https://github.com/Azure/AKSFlexNode/pull/376)).
- Reset now removes LocalDNS state and the nspawn lifecycle helper ([#323](https://github.com/Azure/AKSFlexNode/pull/323)).

## [0.2.0] - 2026-09-21

### Added

- Added scheduled cleanup for stale end-to-end test resources ([#310](https://github.com/Azure/AKSFlexNode/pull/310)).

### Changed

- Install and upgrade the `aks-flex-node` executable using atomic binary replacement ([#308](https://github.com/Azure/AKSFlexNode/pull/308)).
- Reorganized and expanded the design, configuration, getting-started, and operations documentation ([#322](https://github.com/Azure/AKSFlexNode/pull/322)).

### Fixed

- Made release publishing safe to rerun for an existing tag ([#304](https://github.com/Azure/AKSFlexNode/pull/304)).

[Unreleased]: https://github.com/Azure/AKSFlexNode/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/Azure/AKSFlexNode/compare/v0.1.11...v0.2.0
