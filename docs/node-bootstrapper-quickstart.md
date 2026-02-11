# Node Bootstrapper Quickstart

This guide walks through using `node-bootstrapper` to declaratively install packages and manage systemd units on a Linux VM using the overlay system.

## 1. Build the Binary

Cross-compile the binary for Linux from your development machine. The QEMU VM script (`hack/qemu/vm.sh`) creates a VM matching your host architecture, so the default `GOARCH` is correct:

```bash
GOOS=linux go build -o node-bootstrapper ./stretch/cmd/node-bootstrapper
```

The binary has no runtime dependencies beyond D-Bus access (for systemd management).

## 2. Create an Overlay Config

The overlay config is a JSON file that declares which packages to install and which systemd units to manage. Create a file called `overlayconfig.json` in the repo root (it will be accessible inside the VM at `/flex-node/overlayconfig.json`):

```json
{
    "version": "v20260211+1",
    "packagesByName": {
        "containerd": {
            "version": "v2.1.6",
            "source": {
                "type": "url+tar",
                "uri": "https://github.com/containerd/containerd/releases/download/v2.1.6/containerd-2.1.6-linux-arm64.tar.gz"
            }
        },
        "runc": {
            "version": "v1.2.6",
            "source": {
                "type": "url",
                "uri": "https://github.com/opencontainers/runc/releases/download/v1.2.6/runc.arm64"
            }
        }
    },
    "systemdUnitsByName": {
        "containerd": {
            "version": "v1",
            "packages": ["containerd", "runc"],
            "templateInline": "[Unit]\nDescription=containerd container runtime\nAfter=network.target\n\n[Service]\nExecStart={{ .GetPackagePath \"containerd\" \"bin\" \"containerd\" }}\nEnvironment=PATH={{ .GetPathEnvWithSystemDefaults }}\nRestart=always\nRestartSec=5\nDelegate=yes\nKillMode=process\nOOMScoreAdjust=-999\nLimitNOFILE=1048576\nLimitNPROC=infinity\nLimitCORE=infinity\n\n[Install]\nWantedBy=multi-user.target\n"
        }
    }
}
```

### Config structure

**Note:** The example above uses `arm64` package URLs, matching the QEMU VM on Apple Silicon. On x86_64 hosts, replace `arm64` with `amd64` in the download URIs.

| Field | Description |
|---|---|
| `version` | Unique identifier for this config revision. Changing it triggers a new generation. |
| `packagesByName` | Map of package name to definition. Each package has a `version` and a `source`. |
| `systemdUnitsByName` | Map of unit name to definition. Each unit references packages and provides a template. |

### Package source types

| Type | Description |
|---|---|
| `url` | Download a single file |
| `url+tar` | Download and extract a gzipped tarball |
| `url+zip` | Download and extract a zip archive |
| `url+rpm` | Download and extract an RPM package |
| `url+deb` | Download and extract a deb package |
| `file` | Copy a local file or directory |

### Systemd unit templates

Templates use Go `text/template` syntax. The following helpers are available:

| Helper | Description |
|---|---|
| `{{ .GetPathEnv }}` | Colon-joined bin paths from all referenced packages (hermetic, no system paths) |
| `{{ .GetPathEnvWithSystemDefaults }}` | Same as above, plus standard system paths (`/usr/local/bin`, `/usr/bin`, etc.) |
| `{{ .GetPackagePath "name" "sub" "path" }}` | Absolute path into a package's installed directory |

You can use `templateFile` instead of `templateInline` to point to an external template file:

```json
"systemdUnitsByName": {
    "containerd": {
        "version": "v1",
        "packages": ["containerd", "runc"],
        "templateFile": "/path/to/containerd.service.tmpl"
    }
}
```

### Etc files

Packages can declare files to be symlinked into `/etc` via the `etcFiles` field:

```json
"containerd": {
    "version": "v2.1.6",
    "source": { "type": "url+tar", "uri": "https://..." },
    "etcFiles": [
        { "source": "etc/containerd/config.toml", "target": "containerd/config.toml" }
    ]
}
```

- `source` is the relative path inside the extracted package
- `target` is the relative path under `/etc`

## 3. Set Up a VM

The repo includes a QEMU VM script that creates an Ubuntu 24.04 VM with the repo mounted at `/flex-node` via virtio-9p. This means the binary and config are immediately available inside the VM — no manual copying needed.

### Prerequisites

Install QEMU:

```bash
# macOS
brew install qemu cdrtools

# Ubuntu/Debian
sudo apt-get install qemu-system-x86 qemu-utils genisoimage
```

### Start the VM

```bash
./hack/qemu/vm.sh start
```

This downloads an Ubuntu cloud image (first run only), creates a snapshot disk, and boots the VM with hardware acceleration. The script waits for SSH to become available before returning.

Default settings: 2 CPUs, 2 GB RAM, 20 GB disk, SSH on port 2222. Override with flags:

```bash
./hack/qemu/vm.sh start --memory 4096 --cpus 4 --ssh-port 2222
```

### SSH into the VM

```bash
ssh -o StrictHostKeyChecking=no -p 2222 ubuntu@localhost
```

The repo is mounted at `/flex-node`:

```bash
ls /flex-node/node-bootstrapper    # the binary you built in step 1
ls /flex-node/overlayconfig.json          # the config you created in step 2
```

### Other VM commands

```bash
# View serial console logs
./hack/qemu/vm.sh logs
./hack/qemu/vm.sh logs --follow

# Stop the VM
./hack/qemu/vm.sh stop

# Stop and remove all VM artifacts
./hack/qemu/vm.sh stop --force --clean
```

## 4. Run the Bootstrapper

Inside the VM, run `node-bootstrapper` with root privileges (required for systemd management and writing to `/etc`). The binary and config are available via the shared mount at `/flex-node`:

```bash
sudo /flex-node/node-bootstrapper --config-path /flex-node/overlayconfig.json --os-root-dir=/
```

Expected output:

```
Using OS root dir: /
Using store root dir: /aks-flex-node
Installed package runc (ver=v1.2.6, kind=source)
Installed package containerd (ver=v2.1.6, kind=source)
Installed package containerd (ver=v1, kind=systemd-unit)
Installed package etc (ver=v20260211+1, kind=etc-overlay)
2026/02/11 23:34:27 [systemd] daemon-reload
2026/02/11 23:34:27 [systemd] start containerd.service
```

This will:

1. Download and install all declared packages into the store (`/aks-flex-node/states/`)
2. Render systemd unit templates using installed package paths
3. Build a unified `/etc` overlay (symlink tree)
4. Point `/etc/static` at the new overlay
5. Promote overlay entries into `/etc` as symlinks
6. Compute systemd unit deltas (new/changed/removed units)
7. Run `systemctl daemon-reload` and start/restart affected units

### Flags

| Flag | Default | Description |
|---|---|---|
| `--config-path` | (required) | Path to the overlay config JSON file |
| `--os-root-dir` | temp directory | OS root directory (where `/etc` lives). Use `/` for real system changes. |
| `--store-root-dir` | `<os-root-dir>/aks-flex-node` | Store root for packages and state |

## 5. Verify

### Check containerd status

```bash
sudo systemctl status containerd
```

Expected output shows the unit active and running, with the `ExecStart` path pointing into the store:

```
● containerd.service - containerd container runtime
     Loaded: loaded (/etc/systemd/system/containerd.service; linked; preset: enabled)
     Active: active (running) since Wed 2026-02-11 23:34:27 UTC; 7min ago
   Main PID: 1364 (containerd)
      Tasks: 7 (limit: 2294)
     Memory: 12.9M (peak: 13.8M)
        CPU: 1.607s
     CGroup: /system.slice/containerd.service
             └─1364 /aks-flex-node/states/source-containerd-v2.1.6-<hash>/bin/containerd
```

### Check the service file

```bash
sudo systemctl cat containerd.service
```

Expected output shows the rendered unit file with absolute paths into the store:

```
# /etc/systemd/system/containerd.service
[Unit]
Description=containerd container runtime
After=network.target

[Service]
ExecStart=/aks-flex-node/states/source-containerd-v2.1.6-<hash>/bin/containerd
Environment=PATH=/aks-flex-node/states/source-containerd-v2.1.6-<hash>/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
Restart=always
RestartSec=5
Delegate=yes
KillMode=process
OOMScoreAdjust=-999
LimitNOFILE=1048576
LimitNPROC=infinity
LimitCORE=infinity

[Install]
WantedBy=multi-user.target
```

### Inspect the store layout

```bash
ls -la /aks-flex-node/states/
```

You will see fingerprinted directories for each installed package:

```
etc-overlay-etc-v20260211+1-<hash>          # unified etc symlink tree
source-containerd-v2.1.6-<hash>             # extracted containerd binaries
source-runc-v1.2.6-<hash>                   # downloaded runc binary
systemd-unit-containerd-v1-<hash>           # rendered containerd.service file
```

## 6. Updating the Config

To update packages or units, edit `overlayconfig.json` on the host (changes are reflected immediately via the shared mount) and re-run. For example, to upgrade containerd from v2.1.6 to v2.2.1, update the version and URI, and bump the config version:

```json
{
    "version": "v20260211+2",
    "packagesByName": {
        "containerd": {
            "version": "v2.2.1",
            "source": {
                "type": "url+tar",
                "uri": "https://github.com/containerd/containerd/releases/download/v2.2.1/containerd-2.2.1-linux-arm64.tar.gz"
            }
        },
        "runc": {
            "version": "v1.2.6",
            "source": {
                "type": "url",
                "uri": "https://github.com/opencontainers/runc/releases/download/v1.2.6/runc.arm64"
            }
        }
    },
    "systemdUnitsByName": {
        "containerd": {
            "version": "v1",
            "packages": ["containerd", "runc"],
            "templateInline": "[Unit]\nDescription=containerd container runtime\nAfter=network.target\n\n[Service]\nExecStart={{ .GetPackagePath \"containerd\" \"bin\" \"containerd\" }}\nEnvironment=PATH={{ .GetPathEnvWithSystemDefaults }}\nRestart=always\nRestartSec=5\nDelegate=yes\nKillMode=process\nOOMScoreAdjust=-999\nLimitNOFILE=1048576\nLimitNPROC=infinity\nLimitCORE=infinity\n\n[Install]\nWantedBy=multi-user.target\n"
        }
    }
}
```

Then re-run:

```bash
sudo /flex-node/node-bootstrapper --config-path /flex-node/overlayconfig.json --os-root-dir=/
```

Notice that runc is not re-installed (unchanged), and containerd is restarted rather than started:

```
Using OS root dir: /
Using store root dir: /aks-flex-node
Installed package containerd (ver=v2.2.1, kind=source)
Installed package containerd (ver=v1, kind=systemd-unit)
Installed package etc (ver=v20260211+2, kind=etc-overlay)
2026/02/11 23:48:10 [systemd] daemon-reload
2026/02/11 23:48:10 [systemd] restart containerd.service
```

The overlay system handles the transition:

- Unchanged packages are skipped (fingerprint-based caching)
- New or changed systemd units are restarted
- Removed units are stopped
- Stale `/etc` entries from the previous generation are cleaned up

Verify that containerd is now running the new version:

```bash
sudo systemctl status containerd
```

```
● containerd.service - containerd container runtime
     Loaded: loaded (/etc/systemd/system/containerd.service; linked; preset: enabled)
     Active: active (running) since Wed 2026-02-11 23:48:10 UTC; 1min 0s ago
   Main PID: 1475 (containerd)
      Tasks: 8 (limit: 2294)
     Memory: 13.9M (peak: 15.1M)
        CPU: 171ms
     CGroup: /system.slice/containerd.service
             └─1475 /aks-flex-node/states/source-containerd-v2.2.1-<hash>/bin/containerd
```

The `ExecStart` path now points to the v2.2.1 store directory.

Only the `version` field at the top level must change for the system to recognize a new generation.

## 7. Rolling Back

Rolling back is just applying a previous config. Since packages are cached by fingerprint, a rollback re-uses the already-installed store paths and completes almost instantly — no re-downloading.

Revert `overlayconfig.json` to the original v2.1.6 config (bump the version string to indicate a new generation):

```json
{
    "version": "v20260211+3",
    "packagesByName": {
        "containerd": {
            "version": "v2.1.6",
            "source": {
                "type": "url+tar",
                "uri": "https://github.com/containerd/containerd/releases/download/v2.1.6/containerd-2.1.6-linux-arm64.tar.gz"
            }
        },
        "runc": {
            "version": "v1.2.6",
            "source": {
                "type": "url",
                "uri": "https://github.com/opencontainers/runc/releases/download/v1.2.6/runc.arm64"
            }
        }
    },
    "systemdUnitsByName": {
        "containerd": {
            "version": "v1",
            "packages": ["containerd", "runc"],
            "templateInline": "[Unit]\nDescription=containerd container runtime\nAfter=network.target\n\n[Service]\nExecStart={{ .GetPackagePath \"containerd\" \"bin\" \"containerd\" }}\nEnvironment=PATH={{ .GetPathEnvWithSystemDefaults }}\nRestart=always\nRestartSec=5\nDelegate=yes\nKillMode=process\nOOMScoreAdjust=-999\nLimitNOFILE=1048576\nLimitNPROC=infinity\nLimitCORE=infinity\n\n[Install]\nWantedBy=multi-user.target\n"
        }
    }
}
```

Re-run:

```bash
sudo /flex-node/node-bootstrapper --config-path /flex-node/overlayconfig.json --os-root-dir=/
```

Because the v2.1.6 containerd package is still in the store from the original install, it is not re-downloaded — only the etc overlay is rebuilt, and containerd is restarted pointing back to v2.1.6:

```
Using OS root dir: /
Using store root dir: /aks-flex-node
2026/02/11 23:50:58 [systemd] daemon-reload
2026/02/11 23:50:58 [systemd] restart containerd.service
```

No `Installed package` lines — everything was a cache hit. Verify:

```bash
sudo systemctl status containerd
```

```
● containerd.service - containerd container runtime
     Loaded: loaded (/etc/systemd/system/containerd.service; linked; preset: enabled)
     Active: active (running) since Wed 2026-02-11 23:50:58 UTC; 4s ago
   Main PID: 1536 (containerd)
      Tasks: 8 (limit: 2294)
     Memory: 13.0M (peak: 14.0M)
        CPU: 44ms
     CGroup: /system.slice/containerd.service
             └─1536 /aks-flex-node/states/source-containerd-v2.1.6-<hash>/bin/containerd
```

The `ExecStart` path points back to the v2.1.6 store directory.
