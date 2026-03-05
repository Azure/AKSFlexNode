package v20260301

import (
	"context"
	_ "embed"
	"fmt"

	"google.golang.org/protobuf/types/known/anypb"

	"github.com/Azure/AKSFlexNode/components/linux"
	"github.com/Azure/AKSFlexNode/components/services/actions"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/systemd"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilpb"
)

const (
	iptablesFlushUnit = "iptables-flush.service"
	iptablesClearPath = config.ConfigDir + "/iptables-clear"
)

//go:embed assets/iptables-flush.service
var iptablesFlushServiceUnit []byte

//go:embed assets/iptables-clear
var iptablesClearRules []byte

// configureIPTablesAction installs a oneshot systemd unit that flushes all
// iptables rules to a clean ACCEPT state before kubelet starts. This ensures
// stale rules (e.g. left behind by Docker) do not interfere with Kubernetes
// networking.
type configureIPTablesAction struct {
	systemd systemd.Manager
}

func newConfigureIPTablesAction() (actions.Server, error) {
	return &configureIPTablesAction{
		systemd: systemd.New(),
	}, nil
}

var _ actions.Server = (*configureIPTablesAction)(nil)

func (a *configureIPTablesAction) ApplyAction(
	ctx context.Context,
	req *actions.ApplyActionRequest,
) (*actions.ApplyActionResponse, error) {
	settings, err := utilpb.AnyTo[*linux.ConfigureIPTables](req.GetItem())
	if err != nil {
		return nil, err
	}

	if err := a.ensureIPTablesClearRules(); err != nil {
		return nil, fmt.Errorf("installing iptables-clear rules: %w", err)
	}

	if err := a.ensureIPTablesFlushUnit(ctx); err != nil {
		return nil, fmt.Errorf("configuring iptables-flush service: %w", err)
	}

	item, err := anypb.New(settings)
	if err != nil {
		return nil, err
	}

	return actions.ApplyActionResponse_builder{Item: item}.Build(), nil
}

// ensureIPTablesClearRules idempotently writes the iptables-restore rules file
// to /etc/aks-flex/iptables-clear. The file resets all tables (mangle, raw, filter,
// security, nat) to their default ACCEPT policies with empty chains.
func (a *configureIPTablesAction) ensureIPTablesClearRules() error {
	return a.ensureIPTablesClearRulesAt(iptablesClearPath)
}

// ensureIPTablesClearRulesAt writes the iptables-clear rules to the given path.
func (a *configureIPTablesAction) ensureIPTablesClearRulesAt(path string) error {
	return utilio.WriteFile(path, iptablesClearRules, 0600)
}

// ensureIPTablesFlushUnit idempotently installs and enables the
// iptables-flush.service oneshot unit. The unit runs iptables-restore with the
// clean rules file before kubelet.service starts.
func (a *configureIPTablesAction) ensureIPTablesFlushUnit(ctx context.Context) error {
	unitUpdated, err := a.systemd.EnsureUnitFile(ctx, iptablesFlushUnit, iptablesFlushServiceUnit)
	if err != nil {
		return err
	}

	if err := a.systemd.EnableUnit(ctx, iptablesFlushUnit); err != nil {
		return err
	}

	return systemd.EnsureUnitRunning(ctx, a.systemd, iptablesFlushUnit, unitUpdated, unitUpdated)
}
