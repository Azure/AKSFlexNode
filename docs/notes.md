goals:
- reusable node bootstrapping & component management framework
- support both VHD image and non VHD image usage scenarios
- in-place update support

1. config should be versionized to allow for future changes and backward compatibility
2. systemd unit can be handled by https://github.com/coreos/go-systemd/tree/main/unit ?
3. binaries bundle instead of downloading one by one?
4. multiple phase bootstrapping
   - bootstrap0 -> downloading binaries, rendering configs => for building VHD
   - bootstrap1 -> node registration => for individual node bootstrapping
5. reconcile to desire state instead of fresh install every time (canSkipXXX)?
6. in place update support?
7. nixos style management?
  - baseline: versionized configs so we can track changes and roll back if needed
    (maybe #1 is not needed if we have this)
8. activation script to apply changes to the system
9. different architectures support (linux/amd64, linux/arm64)
10. https://github.com/NixOS/nixpkgs/blob/897671288b95e380d318e9cd4427d2c5cbc535ad/pkgs/by-name/sw/switch-to-configuration-ng/src/src/main.rs
11. https://github.com/NixOS/nixpkgs/blob/ba264b1fde602d5d814b0c87d2c951f2ffb636f2/nixos/modules/system/etc/setup-etc.pl
12. https://github.com/NixOS/nixpkgs/blob/0dd2c4bf925900b38b9c0055d3560982834a896f/nixos/lib/systemd-lib.nix#L643
13. https://github.com/NixOS/nixpkgs/blob/nixos-25.11/nixos/modules/system/etc/etc.nix


```
/aks-flex/     <- store root
  |- configs/
     |- v1.yaml
     |- v2.yaml
  |- states/
     |- containerd-<hash1>/
        |- bin/
            | - containerd
     |- containerd-<hash2>/
        |- bin/
            | - containerd
     |- kubelet-<hash3>/
        |- bin/
            | - kubelet
     |- kubelet-<hash4>/
        |- bin/
            | - kubelet
     |- containerd-unit-<hash1>/
     |- kubelet-unit-<hash3>/
     |- kubelet-unit-<hash4>/
     |- system-overlay-<hash5>/
        |- etc/systemd/system/
           |- containerd.service -> /aks-flex/states/containerd-unit-<hash1>/
           |- kubelet.service -> /aks-flex/states/kubelet-unit-<hash3>/
     |- system-overlay-<hash6>/
           |- containerd.service -> /aks-flex/states/containerd-unit-<hash2>/
           |- kubelet.service -> /aks-flex/states/kubelet-unit-<hash4>

/etc/systemd/system/
  |- containerd.service -> /aks-flex/states/containerd-unit-<hash2>/
  |- kubelet.service -> /aks-flex/states/kubelet-unit-<hash4>/
```

activation

- render overlay
- calculate system units change
- symlink files
- apply systemd unit changes