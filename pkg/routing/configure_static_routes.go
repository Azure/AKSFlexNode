// Package routing provides phases.Task implementations that configure IPv4
// routing on the host before kubelet starts.
//
// ConfigureStaticRoutes installs explicit routes to prevent provider-installed
// connected routes (e.g. Azure IB /16 on ND-isr SKUs) from shadowing cluster
// CIDRs.
//
// CheckRouteOverlap verifies that expected CIDRs all route via the IPv4 default
// outbound interface, catching unmitigated routing overlaps at boot time.
package routing

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

const (
	staticRoutesUnit       = "static-routes.service"
	staticRoutesScriptPath = aksFlexNodeConfigDir + "/static-routes.sh"

	// aksFlexNodeConfigDir is the AKSFlexNode configuration directory on the host.
	// Script files are installed here and referenced by their systemd unit.
	aksFlexNodeConfigDir = "/etc/aks-flex-node"
)

//go:embed assets/static-routes.service
var staticRoutesServiceUnit []byte

//go:embed assets/static-routes.sh.tpl
var staticRoutesScriptTemplate string

type configureStaticRoutesTask struct {
	cfg    config.StaticRoutesConfig
	logger *slog.Logger
}

// ConfigureStaticRoutes returns a task that installs a oneshot systemd unit
// which applies static IPv4 routes via `ip -4 route replace` before the nspawn
// machine starts. When no routes are configured the task is a no-op.
//
// This is intended for cases where the VM provider's default routing is wrong
// for the cluster — for example, Azure ND-isr SKUs install connected /16 routes
// for the InfiniBand fabric that can shadow legitimate cluster CIDRs.
// More-specific routes added via this task win over the IB /16 without
// disturbing peer-to-peer IB traffic.
func ConfigureStaticRoutes(cfg *config.Config, logger *slog.Logger) phases.Task {
	return &configureStaticRoutesTask{
		cfg:    cfg.HostRouting.StaticRoutes,
		logger: logger,
	}
}

func (t *configureStaticRoutesTask) Name() string { return "configure-static-routes" }

func (t *configureStaticRoutesTask) Do(ctx context.Context) error {
	if err := validateStaticRoutesConfig(t.cfg); err != nil {
		return fmt.Errorf("configure-static-routes: invalid config: %w", err)
	}

	routes := t.cfg.Routes
	if len(routes) == 0 {
		t.logger.Info("no static routes configured; skipping")
		return nil
	}

	script, err := renderStaticRoutesScript(routes)
	if err != nil {
		return fmt.Errorf("rendering static-routes script: %w", err)
	}

	scriptUpdated, err := writeScriptIfChanged(staticRoutesScriptPath, []byte(script))
	if err != nil {
		return fmt.Errorf("writing static-routes script: %w", err)
	}

	if err := ensureSystemdUnit(ctx, t.logger, staticRoutesUnit, staticRoutesServiceUnit, scriptUpdated); err != nil {
		return fmt.Errorf("configuring static-routes service: %w", err)
	}

	return nil
}

// validateStaticRoutesConfig returns an error when the config is contradictory
// (routes provided but enabled=false).
func validateStaticRoutesConfig(cfg config.StaticRoutesConfig) error {
	if len(cfg.Routes) > 0 && !cfg.Enabled {
		return fmt.Errorf("routes were provided but enabled=false; set enabled=true to apply static routes")
	}
	return nil
}

// renderStaticRoutesScript produces an idempotent bash script that applies each
// route via `ip -4 route replace`. When Dev is empty the script resolves the
// outbound interface from the default IPv4 route at boot time. When Gateway is
// empty the script resolves the default gateway on that dev.
func renderStaticRoutesScript(routes []config.StaticRoute) (string, error) {
	const (
		autoDevToken = "@@AUTO_DEV@@"
		autoGWToken  = "@@AUTO_GW@@"
	)

	type entry struct {
		dest   string
		dev    string
		gw     string
		metric uint32
	}

	entries := make([]entry, 0, len(routes))
	for i, r := range routes {
		prefix, err := netip.ParsePrefix(r.Destination)
		if err != nil {
			return "", fmt.Errorf("route %d: invalid destination %q: %w", i, r.Destination, err)
		}
		if !prefix.Addr().Is4() {
			return "", fmt.Errorf("route %d: destination %q is not IPv4", i, r.Destination)
		}
		if r.Gateway != "" {
			gwAddr, err := netip.ParseAddr(r.Gateway)
			if err != nil {
				return "", fmt.Errorf("route %d: invalid gateway %q: %w", i, r.Gateway, err)
			}
			if !gwAddr.Is4() {
				return "", fmt.Errorf("route %d: gateway %q is not IPv4", i, r.Gateway)
			}
		}
		if r.Dev != "" && !isSafeIfaceName(r.Dev) {
			return "", fmt.Errorf("route %d: invalid dev name %q", i, r.Dev)
		}

		dev := r.Dev
		if dev == "" {
			dev = autoDevToken
		}
		gw := r.Gateway
		if gw == "" {
			gw = autoGWToken
		}
		entries = append(entries, entry{
			dest:   r.Destination,
			dev:    dev,
			gw:     gw,
			metric: r.Metric,
		})
	}

	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		lines = append(lines, fmt.Sprintf("%s|%s|%s|%d", e.dest, e.dev, e.gw, e.metric))
	}

	tmpl, err := template.New("static-routes.sh.tpl").Parse(staticRoutesScriptTemplate)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, map[string]any{
		"AutoDevToken": autoDevToken,
		"AutoGWToken":  autoGWToken,
		"HasEntries":   len(lines) > 0,
		"Entries":      strings.Join(lines, "\n"),
	}); err != nil {
		return "", err
	}

	return out.String(), nil
}

// isSafeIfaceName rejects shell metacharacters to keep the generated script
// safe against malformed config inputs. Linux interface names are ≤15 chars
// of [A-Za-z0-9_.-].
func isSafeIfaceName(s string) bool {
	if len(s) == 0 || len(s) > 15 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '.' || r == '-':
		default:
			return false
		}
	}
	return true
}

// writeScriptIfChanged writes content to path only when the existing file
// differs. Returns true when the file was created or updated.
func writeScriptIfChanged(path string, content []byte) (bool, error) {
	existing, err := os.ReadFile(path) //#nosec G304 -- trusted constant path
	switch {
	case errors.Is(err, os.ErrNotExist):
		// fall through to write
	case err != nil:
		return false, err
	default:
		if bytes.Equal(existing, content) {
			return false, nil
		}
	}
	if err := utilio.WriteFile(path, content, 0o755); err != nil {
		return false, err
	}
	return true, nil
}

// ensureSystemdUnit writes the unit file, daemon-reloads, enables, and starts
// the unit. When restart is true (unit file or script changed) and the unit is
// already active it is restarted so the updated script runs.
func ensureSystemdUnit(ctx context.Context, logger *slog.Logger, unit string, unitContent []byte, restart bool) error {
	unitPath := filepath.Join(goalstates.SystemdSystemDir, unit)

	existing, err := os.ReadFile(unitPath) //#nosec G304 -- trusted constant path
	unitChanged := errors.Is(err, os.ErrNotExist) || !bytes.Equal(existing, unitContent)

	if unitChanged {
		if err := utilio.WriteFile(unitPath, unitContent, 0o644); err != nil {
			return fmt.Errorf("write unit file %s: %w", unitPath, err)
		}
	}

	systemctl := "systemctl"
	if err := runCmd(ctx, logger, systemctl, "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}

	if err := runCmd(ctx, logger, systemctl, "enable", unit); err != nil {
		return fmt.Errorf("systemctl enable %s: %w", unit, err)
	}

	if unitChanged || restart {
		// The unit may be active with RemainAfterExit=yes; restart it so the
		// updated script actually runs.
		if err := runCmd(ctx, logger, systemctl, "restart", unit); err != nil {
			return fmt.Errorf("systemctl restart %s: %w", unit, err)
		}
		return nil
	}

	// Start for the first time; idempotent if already active.
	if err := runCmd(ctx, logger, systemctl, "start", unit); err != nil {
		return fmt.Errorf("systemctl start %s: %w", unit, err)
	}
	return nil
}

// runCmd runs the given command and logs its output via logger.
func runCmd(ctx context.Context, logger *slog.Logger, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // name is a constant binary path
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if stderr.Len() > 0 {
			logger.Error(strings.TrimRight(stderr.String(), "\n"), "cmd", name)
		}
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}
