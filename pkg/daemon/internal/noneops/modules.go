// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package noneops

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	executil "github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

// ModulesLoadPath is where the node's kernel modules are listed for boot. The
// nspawn machine lists them inside its rootfs; a host-native node needs them on
// the host, and the agent-specific name never collides with a distro file. A
// teardown for any mode that wrote it through LoadKernelModules removes it.
const ModulesLoadPath = "/etc/modules-load.d/unbounded-kubernetes.conf"

type kernelModules struct {
	log  *slog.Logger
	run  none.Runner
	path string
}

// LoadKernelModules returns a task that lists the node's kernel modules for
// boot and loads them now. The sysctls the host phase applies (bridge-nf-call)
// need br_netfilter loaded first.
func LoadKernelModules(log *slog.Logger, run none.Runner) *kernelModules {
	return &kernelModules{log: log, run: run, path: ModulesLoadPath}
}

func (k *kernelModules) Name() string { return "load-kernel-modules" }

func (k *kernelModules) Do(ctx context.Context) error {
	path := k.path
	if path == "" {
		path = ModulesLoadPath
	}

	content := rootfs.KubernetesModulesConfig()

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}

	if err := os.WriteFile(path, content, 0o644); err != nil { //nolint:gosec // modules-load.d files are world readable
		return fmt.Errorf("write %s: %w", path, err)
	}

	modules := strings.Fields(string(content))

	run := k.run
	if run == nil {
		run = func(ctx context.Context, log *slog.Logger, name string, args ...string) (string, error) {
			return executil.OutputCmd(ctx, log, name, args...)
		}
	}

	for _, m := range modules {
		if _, err := run(ctx, k.log, "modprobe", m); err != nil {
			return fmt.Errorf("modprobe %s: %w", m, err)
		}
	}

	return nil
}
