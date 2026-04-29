package routing

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"text/template"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

const checkRouteOverlapUnit = "check-route-overlap.service"

// checkRouteOverlapScriptPath is installed to the same config dir as the
// static-routes script so both units share a consistent location.
const checkRouteOverlapScriptPath = aksFlexNodeConfigDir + "/check-route-overlap.sh"

//go:embed assets/check-route-overlap.service
var checkRouteOverlapServiceUnit []byte

//go:embed assets/check-route-overlap.sh.tpl
var checkRouteOverlapScriptTemplate string

type checkRouteOverlapTask struct {
	cfg    config.RouteOverlapConfig
	logger *slog.Logger
}

// CheckRouteOverlap returns a task that installs a oneshot systemd unit which,
// before the nspawn machine starts, verifies that expected IPv4 CIDRs all route
// via the IPv4 default outbound interface.
//
// The classic failure it catches is the Azure ND-isr H200 IB driver shadowing a
// customer VNet CIDR with a connected /16 on ib0 — in that case
// `ip -4 route get <cluster-cidr-probe>` returns ib0 instead of eth0, traffic
// blackholes, and kubelet looks healthy while pods can't reach the API server.
//
// Pair with ConfigureStaticRoutes (which fixes the overlap) for full coverage:
// the static-routes unit is ordered Before this one, so by the time the check
// runs the kernel route table reflects any mitigations.
//
// Mode "STRICT" (recommended for production): any overlap causes the unit to
// exit 1. With RequiredBy=systemd-nspawn@.service the nspawn machine will not
// start until the overlap is resolved. Mode "WARN" (default): the overlap is
// logged and written to /run/aks-flex-node/route-overlap.detected but the
// machine still starts.
func CheckRouteOverlap(cfg *config.Config, logger *slog.Logger) phases.Task {
	return &checkRouteOverlapTask{
		cfg:    cfg.HostRouting.RouteOverlap,
		logger: logger,
	}
}

func (t *checkRouteOverlapTask) Name() string { return "check-route-overlap" }

func (t *checkRouteOverlapTask) Do(ctx context.Context) error {
	mode, err := parseRouteOverlapMode(t.cfg.Mode)
	if err != nil {
		return fmt.Errorf("check-route-overlap: invalid mode: %w", err)
	}

	script, err := renderCheckRouteOverlapScript(t.cfg.ExpectedCIDRs, mode)
	if err != nil {
		return fmt.Errorf("rendering check-route-overlap script: %w", err)
	}

	scriptUpdated, err := writeScriptIfChanged(checkRouteOverlapScriptPath, []byte(script))
	if err != nil {
		return fmt.Errorf("writing check-route-overlap script: %w", err)
	}

	if err := ensureSystemdUnit(ctx, t.logger, checkRouteOverlapUnit, checkRouteOverlapServiceUnit, scriptUpdated); err != nil {
		return fmt.Errorf("configuring check-route-overlap service: %w", err)
	}

	return nil
}

// routeOverlapMode is the typed mode for CheckRouteOverlap.
type routeOverlapMode int

const (
	routeOverlapWarn   routeOverlapMode = iota // default: log and continue
	routeOverlapStrict                         // block kubelet on overlap
)

// parseRouteOverlapMode converts the string from config to a typed mode.
// An empty string defaults to WARN for backward compatibility.
func parseRouteOverlapMode(s string) (routeOverlapMode, error) {
	switch strings.ToUpper(s) {
	case "", "WARN":
		return routeOverlapWarn, nil
	case "STRICT":
		return routeOverlapStrict, nil
	default:
		return 0, fmt.Errorf("unknown mode %q: must be WARN or STRICT", s)
	}
}

// renderCheckRouteOverlapScript produces a bash script that, for each expected
// CIDR, picks a probe address inside the prefix and runs `ip -4 route get
// <probe>` to ask the kernel which interface that address would actually go out.
// Any mismatch with the IPv4 default route's interface is logged; in STRICT mode
// the script then exits 1 and (because the unit is RequiredBy=kubelet.service)
// kubelet does not start.
func renderCheckRouteOverlapScript(cidrs []string, mode routeOverlapMode) (string, error) {
	type entry struct {
		cidr  string
		probe string
	}
	entries := make([]entry, 0, len(cidrs))
	for i, c := range cidrs {
		prefix, err := netip.ParsePrefix(c)
		if err != nil {
			return "", fmt.Errorf("expected_cidrs[%d]: invalid CIDR %q: %w", i, c, err)
		}
		if !prefix.Addr().Is4() {
			return "", fmt.Errorf("expected_cidrs[%d]: %q is not IPv4", i, c)
		}
		maskedPrefix := prefix.Masked()
		// Probe with first usable address (network address + 1). Works for any
		// prefix shorter than /32; for /32 we probe the address itself.
		probe := maskedPrefix.Addr()
		if maskedPrefix.Bits() < 32 {
			if next := probe.Next(); maskedPrefix.Contains(next) {
				probe = next
			}
		}
		entries = append(entries, entry{cidr: c, probe: probe.String()})
	}

	failExit := "0"
	modeLabel := "WARN"
	if mode == routeOverlapStrict {
		failExit = "1"
		modeLabel = "STRICT"
	}

	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		lines = append(lines, fmt.Sprintf("%s|%s", e.cidr, e.probe))
	}

	tmpl, err := template.New("check-route-overlap.sh.tpl").Parse(checkRouteOverlapScriptTemplate)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, map[string]any{
		"ModeLabel":  modeLabel,
		"FailExit":   failExit,
		"HasEntries": len(lines) > 0,
		"Entries":    strings.Join(lines, "\n"),
	}); err != nil {
		return "", err
	}

	return out.String(), nil
}
