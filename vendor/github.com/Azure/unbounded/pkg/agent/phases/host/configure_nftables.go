// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package host

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"path/filepath"
	"text/template"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/utilexec"
	"github.com/Azure/unbounded/pkg/agent/utilio"
)

const (
	nftablesFlushUnit = "nftables-flush.service"
	nftablesClearPath = goalstates.ConfigDir + "/nftables-clear.nft"
)

//go:embed assets/nftables-flush.service
var nftablesFlushServiceTmpl string

var nftablesFlushServiceTemplate = template.Must(
	template.New("nftables-flush.service").Parse(nftablesFlushServiceTmpl),
)

//go:embed assets/nftables-clear.nft
var nftablesClearRules []byte

type configureNFTables struct {
	log *slog.Logger
}

// ConfigureNFTables returns a task that installs a oneshot systemd unit which
// flushes all nftables rules to a clean state before kubelet starts.
// This ensures stale rules (e.g. left behind by Docker) do not interfere with
// Kubernetes networking.
func ConfigureNFTables(log *slog.Logger) phases.Task {
	return &configureNFTables{log: log}
}

func (c *configureNFTables) Name() string { return "configure-nftables" }

func (c *configureNFTables) Do(ctx context.Context) error {
	if err := c.ensureNFTablesClearRules(); err != nil {
		return fmt.Errorf("installing nftables-clear rules: %w", err)
	}

	if err := c.ensureNFTablesFlushUnit(ctx); err != nil {
		return fmt.Errorf("configuring nftables-flush service: %w", err)
	}

	return nil
}

// ensureNFTablesClearRules writes the nftables rules file to
// <ConfigDir>/nftables-clear.nft. The file flushes the entire nftables
// ruleset, which resets all tables to a clean state (nftables defaults to
// accept when no rules are loaded).
func (c *configureNFTables) ensureNFTablesClearRules() error {
	return utilio.WriteFile(nftablesClearPath, nftablesClearRules, 0o600)
}

// ensureNFTablesFlushUnit installs, enables, and starts the nftables-flush.service
// oneshot unit. The unit runs nft with the clean rules file before any
// systemd-nspawn machine starts.
func (c *configureNFTables) ensureNFTablesFlushUnit(ctx context.Context) error {
	var buf bytes.Buffer
	if err := nftablesFlushServiceTemplate.Execute(&buf, map[string]string{
		"NFTablesClearPath": nftablesClearPath,
	}); err != nil {
		return fmt.Errorf("rendering %s template: %w", nftablesFlushUnit, err)
	}

	unitPath := filepath.Join(goalstates.SystemdSystemDir, nftablesFlushUnit)

	if err := utilio.WriteFile(unitPath, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", unitPath, err)
	}

	systemctl := utilexec.Systemctl()

	if err := utilexec.RunCmd(ctx, c.log, systemctl, "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}

	if err := utilexec.RunCmd(ctx, c.log, systemctl, "enable", nftablesFlushUnit); err != nil {
		return fmt.Errorf("systemctl enable %s: %w", nftablesFlushUnit, err)
	}

	if err := utilexec.RunCmd(ctx, c.log, systemctl, "start", nftablesFlushUnit); err != nil {
		return fmt.Errorf("systemctl start %s: %w", nftablesFlushUnit, err)
	}

	return nil
}
