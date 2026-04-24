// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package host

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/utilexec"
	"github.com/Azure/unbounded/pkg/agent/utilio"
)

// requiredPackages lists the OS packages that must be installed on the host.
// - systemd-container: provides systemd-nspawn for running containers.
// - debootstrap: used to bootstrap a Debian rootfs.
// - curl: used for downloading resources.
// - nftables: provides nft, used by nftables-flush.service to reset firewall rules.
var requiredPackages = []string{
	"systemd-container",
	"debootstrap",
	"curl",
	"nftables",
}

type installPackages struct {
	log *slog.Logger
}

// InstallPackages returns a task that installs the required OS packages on the host.
//
// TODO: support package managers beyond apt (e.g. dnf, zypper) for non-Debian distros.
func InstallPackages(log *slog.Logger) phases.Task {
	return &installPackages{log: log}
}

func (ip *installPackages) Name() string { return "install-packages" }

func (ip *installPackages) Do(ctx context.Context) error {
	aptGet := utilexec.AptGet()

	var missing []string

	for _, pkg := range requiredPackages {
		if !isDebianPackageInstalled(ctx, ip.log, pkg) {
			missing = append(missing, pkg)
		}
	}

	if len(missing) == 0 {
		return nil
	}

	// Refresh the package index before installing.
	if err := utilexec.RunCmd(ctx, ip.log, aptGet, "update", "-y"); err != nil {
		return fmt.Errorf("apt-get update: %w", err)
	}

	// Install all missing packages in a single invocation.
	args := append([]string{"install", "-y", "--no-install-recommends"}, missing...)

	if err := utilexec.RunCmd(ctx, ip.log, aptGet, args...); err != nil {
		return fmt.Errorf("apt-get install %s: %w", strings.Join(missing, " "), err)
	}

	return nil
}

// isDebianPackageInstalled checks whether a package is fully installed using dpkg-query.
func isDebianPackageInstalled(ctx context.Context, log *slog.Logger, pkg string) bool {
	output, err := utilexec.OutputCmd(ctx, log, "dpkg-query", "--show", "--showformat=${db:Status-Status}", pkg)
	if err != nil {
		return false
	}

	return strings.TrimSpace(output) == "installed"
}

// Kubernetes sysctl settings. Inside systemd-nspawn, /proc/sys is a read-only
// bind mount of the host's /proc/sys, so these must be applied on the host
// before kubelet starts. kubelet's ContainerManager (with
// --protect-kernel-defaults=true) verifies the expected values on startup and
// refuses to start if they are incorrect.
//
//go:embed assets/99-kubernetes-sysctl.conf
var kubernetesSysctlConfig []byte

const hostSysctlPath = "/etc/sysctl.d/99-kubernetes.conf"

type configureOS struct {
	log *slog.Logger
}

// ConfigureOS returns a task that writes host-level OS configuration (e.g. sysctl tunables)
// that must be in place before any nspawn machine starts so that kubelet inside the
// container sees the correct kernel parameter values.
func ConfigureOS(log *slog.Logger) phases.Task {
	return &configureOS{log: log}
}

func (c *configureOS) Name() string { return "configure-os" }

func (c *configureOS) Do(ctx context.Context) error {
	if err := utilio.WriteFile(hostSysctlPath, kubernetesSysctlConfig, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", hostSysctlPath, err)
	}

	if err := utilexec.RunCmd(ctx, c.log, utilexec.Sysctl(), "--system"); err != nil {
		return fmt.Errorf("sysctl --system: %w", err)
	}

	return nil
}
