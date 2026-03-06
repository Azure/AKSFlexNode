package v20260301

import (
	"context"
	_ "embed"

	"github.com/Azure/AKSFlexNode/pkg/systemd"
)

const systemdUnitResolvUplinkOverride = "resolv-uplink-override.service"

//go:embed assets/resolv-uplink-override.service
var resolvUplinkOverrideUnit []byte

// disableSystemdResolvedCache creates and enables a systemd oneshot service that
// symlinks /etc/resolv.conf to /run/systemd/resolve/resolv.conf. This bypasses
// the systemd-resolved stub resolver cache, ensuring kubelet and pods use the
// upstream DNS servers directly.
//
// See: https://kubernetes.io/docs/tasks/administer-cluster/dns-debugging-resolution/
func (a *configureBaseOSAction) disableSystemdResolvedCache(ctx context.Context) error {
	unitUpdated, err := a.systemd.EnsureUnitFile(ctx, systemdUnitResolvUplinkOverride, resolvUplinkOverrideUnit)
	if err != nil {
		return err
	}

	return systemd.EnsureUnitRunning(ctx, a.systemd, systemdUnitResolvUplinkOverride, unitUpdated, unitUpdated)
}
