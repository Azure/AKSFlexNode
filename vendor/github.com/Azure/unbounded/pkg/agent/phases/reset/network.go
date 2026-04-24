// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package reset

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/utilexec"
)

// knownOverlayInterfaces lists the tunnel and overlay interfaces created by
// unbounded-net that must be removed during reset.
var knownOverlayInterfaces = []string{
	"geneve0",
	"vxlan0",
	"ipip0",
	"unbounded0",
	"cbr0",
}

type removeNetworkInterfaces struct {
	log *slog.Logger
}

// RemoveNetworkInterfaces returns a task that removes all network interfaces
// created by unbounded-net: WireGuard interfaces (wg*), and tunnel/overlay
// interfaces (geneve0, vxlan0, ipip0, unbounded0, cbr0).
func RemoveNetworkInterfaces(log *slog.Logger) phases.Task {
	return &removeNetworkInterfaces{log: log}
}

func (t *removeNetworkInterfaces) Name() string { return "remove-network-interfaces" }

func (t *removeNetworkInterfaces) Do(ctx context.Context) error {
	// Remove WireGuard interfaces (wg51820, wg51821, ...).
	wgIfaces, err := listWireGuardInterfaces(ctx, t.log)
	if err != nil {
		t.log.Warn("failed to list WireGuard interfaces", "error", err)
	}

	for _, iface := range wgIfaces {
		t.log.Info("removing interface", "interface", iface)
		deleteLink(ctx, t.log, iface)
	}

	// Remove tunnel and overlay interfaces.
	for _, iface := range knownOverlayInterfaces {
		if linkExists(t.log, iface) {
			t.log.Info("removing interface", "interface", iface)
			deleteLink(ctx, t.log, iface)
		}
	}

	return nil
}

type removeWireGuardKeys struct {
	log *slog.Logger
}

// RemoveWireGuardKeys returns a task that removes WireGuard private and public
// key files from /etc/wireguard.
func RemoveWireGuardKeys(log *slog.Logger) phases.Task {
	return &removeWireGuardKeys{log: log}
}

func (t *removeWireGuardKeys) Name() string { return "remove-wireguard-keys" }

func (t *removeWireGuardKeys) Do(_ context.Context) error {
	t.log.Info("removing WireGuard keys")

	for _, path := range []string{
		"/etc/wireguard/server.priv",
		"/etc/wireguard/server.pub",
	} {
		removeFileIfExists(t.log, path)
	}

	return nil
}

// listWireGuardInterfaces returns the names of all WireGuard interfaces (names
// matching wg[0-9]*) visible on the host.
func listWireGuardInterfaces(ctx context.Context, log *slog.Logger) ([]string, error) {
	out, err := utilexec.OutputCmd(ctx, log, "ip", "-o", "link", "show")
	if err != nil {
		return nil, fmt.Errorf("ip link show: %w", err)
	}

	var ifaces []string

	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		// Each line looks like: "2: wg51820: <...> ..."
		// The interface name is the second field, with a trailing colon.
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}

		name := strings.TrimRight(fields[1], ":")
		if isWireGuardInterface(name) {
			ifaces = append(ifaces, name)
		}
	}

	return ifaces, nil
}

// isWireGuardInterface returns true if the interface name matches the wg[0-9]+
// pattern used by unbounded-net.
func isWireGuardInterface(name string) bool {
	if !strings.HasPrefix(name, "wg") {
		return false
	}

	suffix := name[2:]
	if suffix == "" {
		return false
	}

	for _, c := range suffix {
		if c < '0' || c > '9' {
			return false
		}
	}

	return true
}

// linkExists checks whether a network interface exists by looking up its
// entry in /sys/class/net. This avoids shelling out and cleanly distinguishes
// "not found" from real errors.
func linkExists(log *slog.Logger, name string) bool {
	_, err := os.Stat(fmt.Sprintf("/sys/class/net/%s", name))
	if err == nil {
		return true
	}

	if errors.Is(err, os.ErrNotExist) {
		return false
	}

	log.Warn("failed to check interface existence", "interface", name, "error", err)

	return false
}

// deleteLink removes a network interface, logging a warning if the operation
// fails (e.g. the interface was already removed).
func deleteLink(ctx context.Context, log *slog.Logger, name string) {
	if err := utilexec.RunCmd(ctx, log, utilexec.Ip(), "link", "delete", name); err != nil {
		log.Warn("failed to delete interface (may already be gone)", "interface", name, "error", err)
	}
}
