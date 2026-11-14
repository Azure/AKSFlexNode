package daemon

import (
	"path/filepath"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/npd"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/noneops/transaction"
)

// Flex owns applied state and NPD configuration; the shared transaction engine
// owns worker preparation, boot gating, startup and recovery.
func newNoneRuntime(p transaction.Params) *transaction.Node {
	p.StateDir = config.ConfigDir
	p.ExtraSnapshotPaths = append(p.ExtraSnapshotPaths,
		"/etc/node-problem-detector/kernel-monitor.json",
		filepath.Join(goalstates.SystemdSystemDir, npd.HostUnitNPD),
		filepath.Join(goalstates.SystemdSystemDir, npd.HostUnitNPD+".d"),
		filepath.Join(goalstates.SystemdSystemDir, "static-routes.service"),
		filepath.Join(goalstates.SystemdSystemDir, "check-route-overlap.service"),
		filepath.Join(config.ConfigDir, "static-routes.sh"),
		filepath.Join(config.ConfigDir, "check-route-overlap.sh"),
	)
	return transaction.New(p)
}
