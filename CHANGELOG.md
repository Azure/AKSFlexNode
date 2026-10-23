# Changelog

All notable user-facing changes to AKS Flex Node are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added

- Disconnect tolerance: when AKS enables it for a node, the agent downloads and runs OpenYurt YurtHub in the worker and routes kubelet through it, so workloads keep running while the node is disconnected from the API server. Enabling it requires outbound HTTPS to `packages.microsoft.com`; joining does not. State is reported as a `DisconnectTolerance` Node condition on every node ([#390](https://github.com/Azure/AKSFlexNode/pull/390), [#391](https://github.com/Azure/AKSFlexNode/pull/391), [#392](https://github.com/Azure/AKSFlexNode/pull/392)).
- `agent.machineClient.apiVersion` overrides the ARM Machine API version ([#390](https://github.com/Azure/AKSFlexNode/pull/390)).

### Changed

- **Breaking change:** `AgentUpgrade` MachineOperations now require `parameters.sha256`, the SHA-256 of the compressed release archive. Requests with missing, blank, or malformed digests fail with `InvalidParameters`; mismatched digests fail before extraction or candidate execution. Update upgrade controllers and scripts to supply the digest for each target version and architecture from the release's `checksums.txt` before deploying agents with this requirement.

### Fixed

- Uninstall the agent service only after disabling it, removing its unit files, and reloading systemd, preventing self-uninstall from leaving stale systemd state ([#371](https://github.com/Azure/AKSFlexNode/pull/371)).
- Re-running `bootstrap.sh` on a host that was reset now installs the downloaded agent instead of leaving the service on the previous binary. Like `install.sh`, it removes the binary layout reset keeps, and refuses while the agent service is installed or active ([#376](https://github.com/Azure/AKSFlexNode/pull/376)).

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
