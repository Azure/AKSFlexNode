// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"context"
	_ "embed"
	"fmt"
	"path/filepath"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/utilio"
)

// /dev/kmsg workaround: kubelet expects /dev/kmsg for kernel log access, but
// systemd-nspawn does not expose it. A tmpfiles rule creates the symlink on
// every boot.
//
//go:embed assets/kmsg.conf
var kmsgConfig []byte

// Kernel module loading config. The host kernel provides the modules; the
// container just needs modprobe to load them at boot.
//
//go:embed assets/kubernetes-modules.conf
var kubernetesModulesConfig []byte

// Embedded apt sources.list files for Ubuntu Noble, one per architecture.
// These are hard-coded to "noble" at this moment.
//
//go:embed assets/sources-amd64.list
var sourcesAmd64 []byte

//go:embed assets/sources-arm64.list
var sourcesArm64 []byte

// archSources maps GOARCH values to the corresponding embedded sources.list content.
var archSources = map[string][]byte{
	"amd64": sourcesAmd64,
	"arm64": sourcesArm64,
}

// osConfigFile describes a configuration file to write into the machine rootfs.
type osConfigFile struct {
	// path is relative to the machine directory root.
	path    string
	content []byte
}

type configureOS struct {
	goalState *goalstates.RootFS
}

// ConfigureOS returns a task that writes OS-level configuration files into the machine rootfs
// so that kubelet and container networking work correctly inside systemd-nspawn.
// This includes an arch-specific apt sources.list so that packages can be
// installed inside the machine during the nodestart phase.
//
// NOTE: The apt sources are hard-coded to Ubuntu Noble at this moment.
func ConfigureOS(goalState *goalstates.RootFS) phases.Task {
	return &configureOS{goalState: goalState}
}

func (c *configureOS) Name() string { return "configure-os" }

func (c *configureOS) Do(_ context.Context) error {
	// Select the sources.list content for the host architecture.
	sources, ok := archSources[c.goalState.HostArch]
	if !ok {
		return fmt.Errorf("unsupported architecture %q: no apt sources available", c.goalState.HostArch)
	}

	configs := []osConfigFile{
		{path: "etc/tmpfiles.d/kmsg.conf", content: kmsgConfig},
		{path: "etc/modules-load.d/kubernetes.conf", content: kubernetesModulesConfig},
		{path: "etc/apt/sources.list", content: sources},
		// Inherit the hostname from the host so that the nspawn container
		// identifies itself with the same name as the underlying machine.
		{path: "etc/hostname", content: []byte(c.goalState.Hostname + "\n")},
	}

	for _, f := range configs {
		dest := filepath.Join(c.goalState.MachineDir, f.path)
		if err := utilio.WriteFile(dest, f.content, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", dest, err)
		}
	}

	return nil
}
