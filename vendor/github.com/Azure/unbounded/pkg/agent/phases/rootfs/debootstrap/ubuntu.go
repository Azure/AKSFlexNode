// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package debootstrap

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/utilexec"
	"github.com/Azure/unbounded/pkg/agent/utilio"
)

type ubuntu struct {
	log        *slog.Logger
	machineDir string
}

// Ubuntu returns a task that bootstraps an Ubuntu rootfs into the machine
// directory using debootstrap.
//
// NOTE: This currently uses debootstrap with the "noble" suite and is Ubuntu-only.
func Ubuntu(log *slog.Logger, machineDir string) phases.Task {
	return &ubuntu{log: log, machineDir: machineDir}
}

func (b *ubuntu) Name() string { return "debootstrap-ubuntu-noble" }

func (b *ubuntu) Do(ctx context.Context) error {
	empty, err := utilio.IsDirEmpty(b.machineDir)
	if err != nil {
		return fmt.Errorf("check machine directory %s: %w", b.machineDir, err)
	}

	if !empty {
		b.log.Warn("machine directory is not empty, skipping debootstrap", slog.String("dir", b.machineDir))
		return nil
	}

	// NOTE: static folder used to allow reusing the cache for repeated run
	const cacheDir = "/tmp/unbounded-agent/debootstrap-cache/noble"
	if err := os.MkdirAll(cacheDir, 0o644); err != nil {
		return err
	}

	// debootstrap is Ubuntu-only: it bootstraps an Ubuntu "noble" rootfs with
	// the minimum set of packages required to run a Kubernetes node.
	packages := []string{
		"systemd",
		"systemd-sysv",
		"dbus",
		"curl",
		"ca-certificates",
		"iproute2",
		"nftables",
		"kmod",
		"udev",
		"procps",
		"nano",
		"wireguard-tools",
	}

	if err := utilexec.RunCmd(ctx, b.log, debootstrapCmd,
		"--include="+strings.Join(packages, ","),
		"--cache-dir="+cacheDir,
		"noble",
		b.machineDir,
	); err != nil {
		return fmt.Errorf("debootstrap into %s: %w", b.machineDir, err)
	}

	return nil
}

func debootstrapCmd(ctx context.Context) *exec.Cmd {
	return exec.CommandContext(ctx, "debootstrap")
}
