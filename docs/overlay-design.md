# Overlay Package System — Design Document

## Overview

The `pkg/units/` package implements a NixOS-inspired declarative package and configuration management system for AKS Flex Node. It replaces imperative component installation (download, extract, copy, configure) with a content-addressed store where packages are immutable, identified by fingerprint, and composed into a unified `/etc` overlay via symlinks.

The system has two phases:

1. **Prepare** (build phase) — download packages, render systemd unit templates, produce a unified etc symlink tree. All artifacts land in the store under `/aks-flex/states/`. This phase is fully implemented.
2. **Apply** (activation phase) — project the etc overlay into the live `/etc`, reload systemd, start/restart units. This phase is not yet implemented.

---

## Store Layout

```
/aks-flex/                          <- store root (configurable)
  ├── configs/
  │   ├── v1.json                   <- versioned overlay config snapshots
  │   └── v2.json
  └── states/
      ├── containerd-<hash>/        <- source package (binaries)
      │   └── bin/containerd
      ├── runc-<hash>/              <- source package
      │   └── bin/runc
      ├── containerd-unit-<hash>/   <- systemd unit package (rendered template)
      │   └── containerd.service
      ├── kubelet-unit-<hash>/
      │   └── kubelet.service
      └── etc-<hash>/               <- etc overlay package (symlink tree)
          └── etc/
              ├── containerd/
              │   └── config.toml -> /aks-flex/states/containerd-<hash>/etc/containerd/config.toml
              └── systemd/system/
                  ├── containerd.service -> /aks-flex/states/containerd-unit-<hash>/containerd.service
                  └── kubelet.service -> /aks-flex/states/kubelet-unit-<hash>/kubelet.service
```

## Core Concepts

### Package

The `Package` interface is the central abstraction. Every artifact in the store — binaries, rendered systemd units, and the etc overlay itself — is a Package.

```go
type Package interface {
    Kind() string                                // "source", "systemd-unit", "etc-overlay"
    Name() string                                // e.g. "containerd", "kubelet"
    Version() string                             // e.g. "1.6.21"
    Sources() []string                           // inputs for fingerprinting
    Install(ctx context.Context, base string) error
    EtcFiles() []PackageEtcFile                  // files to place in /etc
}
```

Three implementations exist:

| Implementation | Kind | What It Does |
|---|---|---|
| `overlayPackage` | `source` | Downloads/extracts binaries from URL, tar, zip, or local file |
| `systemdUnitPackage` | `systemd-unit` | Renders a Go `text/template` with package path helpers |
| `etcOverlayPackage` | `etc-overlay` | Collects EtcFiles from all packages into a unified symlink tree |

### Fingerprinting and Caching

Each package gets a deterministic fingerprint: `SHA-256(name | version | sorted-sources | sorted-etcfiles)`, encoded as lowercase base32 without padding. The state directory is named `<name>-<fingerprint>`.

If the state directory already exists, installation is skipped entirely. This makes Prepare idempotent — re-running with the same config is a no-op.

### Atomic Installation

Packages are installed into a temp directory under `states/`, then atomically renamed (`os.Rename`) to their final fingerprinted path. This ensures that either a complete package exists at the expected path or it doesn't exist at all. Partial installations are cleaned up on failure.

### OverlayConfig

The input to the system is a declarative JSON config:

```json
{
  "version": "v1",
  "packageByNames": {
    "containerd": {
      "version": "1.7.0",
      "source": { "type": "url+tar", "uri": "https://..." },
      "etcFiles": [
        { "source": "etc/containerd/config.toml", "target": "containerd/config.toml" }
      ]
    }
  },
  "systemdUnitsByName": {
    "containerd": {
      "version": "1",
      "packages": ["containerd"],
      "templateInline": "[Unit]\nDescription=containerd\n[Service]\nExecStart={{.GetPackagePath \"containerd\" \"bin\" \"containerd\"}}\nEnvironment=PATH={{.GetPathEnv}}\n"
    }
  }
}
```

---

## Prepare Phase (Implemented)

`Overlay.Prepare(ctx)` orchestrates the build in three steps:

```
OverlayConfig
    │
    ▼
┌─────────────────────────┐
│ 1. prepareOverlayPackages │  Install source packages (download, extract)
│    returns map[name]*InstalledPackage
└───────────┬─────────────┘
            │
            ▼
┌─────────────────────────┐
│ 2. prepareSystemdUnits    │  Resolve templates + package deps, render, install
│    returns []*InstalledPackage
└───────────┬─────────────┘
            │
            ▼
┌─────────────────────────┐
│ 3. prepareEtcOverlay      │  Merge all EtcFiles into unified symlink tree
│    returns *InstalledPackage (the etc overlay)
└───────────┬─────────────┘
            │
            ▼
      *InstalledPackage
      (etc overlay with InstalledStatePath)
```

### Systemd Unit Templates

Systemd unit templates use Go `text/template` with a `unitTemplateContext` that provides:

| Helper | Description |
|---|---|
| `GetPathEnv` | Hermetic PATH — only declared package bin dirs, sorted |
| `GetPathEnvWithSystemDefaults` | PATH + `/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin` |
| `GetPackagePath "name" "sub" "paths"...` | Absolute path into a package's state dir |

### Etc Overlay Construction

The `etcOverlayPackage` collects `EtcFiles()` from all installed packages (source + systemd unit) and creates symlinks:

```
<state>/etc/<target> -> <package-state-dir>/<source>
```

**Conflict policy:** If two packages declare the same target path, it is a hard error naming both packages. There is no implicit precedence or merge strategy.

---

## Apply Phase (Not Yet Implemented)

`Overlay.Apply(ctx)` is currently a stub returning nil. The activation phase needs to:

### 1. Project etc overlay into live /etc

The etc overlay state dir contains the desired symlink tree. Apply must make `/etc` reflect it. The design sketched in `etc.go` uses a two-step indirection similar to NixOS:

- **SymlinkToStatic**: Point `/aks-flex/etc/static` → the etc overlay state dir. This is the "current generation" pointer.
- **PromoteStaticToEtc**: For each entry in the overlay, create `/etc/<target>` → `/aks-flex/etc/static/etc/<target>`.

The indirection through `/etc/static` means switching generations only requires updating one symlink, and individual `/etc` entries always resolve through it.

### 2. Clean up stale entries

When the overlay config changes (e.g., a package is removed or an etc target is renamed), old `/etc` entries must be removed. This requires knowing what the previous generation installed.

Options under consideration:

- **Manifest file** — write a JSON manifest listing all managed `/etc` paths. On next Apply, diff old vs new, remove entries not in the new set.
- **Scan-based** — scan `/etc` for symlinks pointing into `/aks-flex/states/`. Remove any that don't match the current overlay. No external state needed.
- **Generation directory** — track generations in `/aks-flex/generations/` with numbered symlinks (enables rollback).

### 3. Manage systemd units

The `systemdManager` interface is defined in `systemd.go` but not implemented:

```go
type systemdManager interface {
    ReloadDaemon() error                                        // systemctl daemon-reload
    StartUnit(name string) error                                // systemctl start
    RestartUnit(name string) error                              // systemctl restart
    ReloadUnit(name string) error                               // systemctl reload
    GenerateDeltas(oldPath, newPath string) (*systemdUnitDeltas, error)
}
```

`GenerateDeltas` would diff the previous and new etc overlay's `systemd/system/` directory to determine which units need starting, restarting, or reloading. The `systemdUnitDeltas` struct captures this:

```go
type systemdUnitDeltas struct {
    UnitToStart   []string
    UnitToRestart []string
    UnitToReload  []string
}
```

### Proposed Apply sequence

```
Apply(ctx)
  │
  ├── 1. Read previous manifest (if any) to get old etc entries
  │
  ├── 2. Update /aks-flex/etc/static -> new etc overlay state dir
  │
  ├── 3. For each entry in new overlay:
  │       create /etc/<target> -> /aks-flex/etc/static/etc/<target>
  │
  ├── 4. For each entry in old manifest NOT in new overlay:
  │       remove /etc/<target>
  │
  ├── 5. Write new manifest
  │
  ├── 6. GenerateDeltas(old systemd dir, new systemd dir)
  │
  ├── 7. systemctl daemon-reload
  │
  └── 8. Start/restart/reload changed units
```

---

## Design Decisions

| Decision | Rationale |
|---|---|
| Everything is a Package | Uniform fingerprinting, caching, and atomic install for all artifact types |
| Fingerprint-based caching | Same inputs = same outputs; skip work on re-apply |
| Atomic rename for installs | No partial state on disk; crash-safe |
| Hard error on duplicate etc targets | Simpler than NixOS's "tolerate identical" policy; forces explicit resolution |
| Hermetic PATH by default | Systemd units only see declared package binaries; no ambient system leakage |
| Sources use `<kind>://<name>` format | Etc overlay fingerprint captures package identity; each package's own fingerprint captures its content details |
| Prepare returns `*InstalledPackage` | Caller (Apply) gets the etc overlay's state path directly |

## NixOS Comparison

| Aspect | NixOS | AKS Flex Overlay |
|---|---|---|
| Store | `/nix/store/<hash>-<name>` | `/aks-flex/states/<name>-<hash>` |
| Etc overlay | `/nix/store/<hash>-etc/etc/` | `/aks-flex/states/etc-<hash>/etc/` |
| Activation | `switch-to-configuration` (Rust) + `setup-etc.pl` (Perl) | `Overlay.Apply()` (Go) — not yet implemented |
| Symlink strategy | `/etc/static` → store, `/etc/<f>` → `/etc/static/<f>` | Same pattern planned (see `etc.go` stubs) |
| Systemd diffing | `switch-to-configuration-ng` diffs unit files | `systemdManager.GenerateDeltas` — not yet implemented |
| Duplicate etc targets | Tolerated if identical content | Hard error |
| Generations/rollback | Numbered profiles in `/nix/var/nix/profiles/` | Not yet implemented |
| Garbage collection | `nix-collect-garbage` removes unreferenced store paths | Not yet implemented |

## What's Implemented (37 tests passing)

- [x] `StoreManager` — disk layout, config persistence, atomic package install, fingerprint caching
- [x] `overlayPackage` — URL, URL+tar, URL+zip, file/directory sources with path traversal protection
- [x] `systemdUnitPackage` — template rendering with `GetPathEnv`, `GetPathEnvWithSystemDefaults`, `GetPackagePath`
- [x] `etcOverlayPackage` — unified symlink tree with duplicate target conflict detection
- [x] `Overlay.Prepare()` — full pipeline: source packages → systemd units → etc overlay
- [x] `InstalledPackage` — exported type with `BinPaths()` helper

## What's Not Yet Implemented

- [ ] `Overlay.Apply()` — activation phase (project into /etc, manage systemd)
- [ ] `etcManager` — `SymlinkToStatic`, `PromoteStaticToEtc` (stubs exist)
- [ ] `systemdManager` — interface defined, no implementation
- [ ] Stale entry cleanup / generation tracking
- [ ] Garbage collection of unused store paths
- [ ] Mode/permissions on etc files (all are symlinks currently)
- [ ] Checksum validation on package sources (`OverlayPackageSource` has a TODO)
- [ ] Concurrent package installation (TODO in `prepareOverlayPackages`)
- [ ] Integration with existing `pkg/bootstrapper/` pipeline
