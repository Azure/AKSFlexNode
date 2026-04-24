// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package reset

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/utilexec"
)

// wireguardTableStart and wireguardTableEnd define the range of routing table
// IDs used by unbounded-net WireGuard gateways.
const (
	wireguardTableStart = 51820
	wireguardTableEnd   = 51899
)

type cleanupRoutes struct {
	log *slog.Logger
}

// CleanupRoutes returns a task that removes policy routing rules and flushes
// routing tables used by unbounded-net WireGuard gateways.
func CleanupRoutes(log *slog.Logger) phases.Task {
	return &cleanupRoutes{log: log}
}

func (t *cleanupRoutes) Name() string { return "cleanup-routes" }

func (t *cleanupRoutes) Do(ctx context.Context) error {
	t.log.Info("cleaning up policy routing rules")

	for table := wireguardTableStart; table <= wireguardTableEnd; table++ {
		tableStr := fmt.Sprintf("%d", table)

		// Remove all ip rules pointing to this table.
		for {
			if err := utilexec.RunCmd(ctx, t.log, utilexec.Ip(), "rule", "del", "table", tableStr); err != nil {
				break // no more rules for this table
			}
		}

		// Flush the routing table.
		if err := utilexec.RunCmd(ctx, t.log, utilexec.Ip(), "route", "flush", "table", tableStr); err != nil {
			t.log.Warn("failed to flush routing table (may be empty)", "table", tableStr, "error", err)
		}
	}

	return nil
}
