# Changelog

All notable user-facing changes to AKS Flex Node are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Changed

- **Breaking change:** `AgentUpgrade` MachineOperations now require `parameters.sha256`, the SHA-256 of the compressed release archive. Requests with missing, blank, or malformed digests fail with `InvalidParameters`; mismatched digests fail before extraction or candidate execution. Update upgrade controllers and scripts to supply the digest for each target version and architecture from the release's `checksums.txt` before deploying agents with this requirement.

### Fixed

- Uninstall the agent service only after disabling it, removing its unit files, and reloading systemd, preventing self-uninstall from leaving stale systemd state ([#371](https://github.com/Azure/AKSFlexNode/pull/371)).

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
