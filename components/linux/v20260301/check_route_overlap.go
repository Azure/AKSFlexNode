package v20260301

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"net/netip"
	"strings"
	"text/template"

	"google.golang.org/protobuf/types/known/anypb"

	"github.com/Azure/AKSFlexNode/components/linux"
	"github.com/Azure/AKSFlexNode/components/services/actions"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/systemd"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilpb"
)

const (
	checkRouteOverlapUnit       = "check-route-overlap.service"
	checkRouteOverlapScriptPath = config.ConfigDir + "/check-route-overlap.sh"
)

//go:embed assets/check-route-overlap.service
var checkRouteOverlapServiceUnit []byte

//go:embed assets/check-route-overlap.sh.tpl
var checkRouteOverlapScriptTemplate string

// checkRouteOverlapAction installs a oneshot systemd unit that, before
// kubelet starts, verifies that a list of expected IPv4 CIDRs all route
// via the IPv4 default route's outbound interface. The classic failure
// it catches is the Azure ND-isr H200 IB driver shadowing a customer
// VNet CIDR with a connected /16 on ib0 — in that case `ip -4 route get
// <pod-cidr-address>` returns ib0 instead of eth0, traffic blackholes,
// and kubelet looks healthy while pods can't reach the API server.
//
// Pair with ConfigureStaticRoutes (which fixes the overlap) for full
// coverage: the static-routes oneshot is ordered Before this one, so by
// the time the check runs the kernel route table reflects any
// mitigations.
type checkRouteOverlapAction struct {
	systemd systemd.Manager
}

func newCheckRouteOverlapAction() (actions.Server, error) {
	return &checkRouteOverlapAction{
		systemd: systemd.New(),
	}, nil
}

var _ actions.Server = (*checkRouteOverlapAction)(nil)

func (a *checkRouteOverlapAction) ApplyAction(
	ctx context.Context,
	req *actions.ApplyActionRequest,
) (*actions.ApplyActionResponse, error) {
	settings, err := utilpb.AnyTo[*linux.CheckRouteOverlap](req.GetItem())
	if err != nil {
		return nil, err
	}

	spec := settings.GetSpec()
	if err := validateCheckRouteOverlapSpec(spec); err != nil {
		return nil, fmt.Errorf("validating CheckRouteOverlap spec: %w", err)
	}
	mode := spec.GetMode()
	if mode == linux.CheckRouteOverlapSpec_MODE_UNSPECIFIED {
		mode = linux.CheckRouteOverlapSpec_WARN
	}

	script, err := renderCheckRouteOverlapScript(spec.GetExpectedCidrs(), mode)
	if err != nil {
		return nil, fmt.Errorf("rendering check-route-overlap script: %w", err)
	}

	scriptUpdated, err := writeScriptIfChanged(checkRouteOverlapScriptPath, []byte(script))
	if err != nil {
		return nil, fmt.Errorf("writing check-route-overlap script: %w", err)
	}

	if err := a.ensureCheckRouteOverlapUnit(ctx, scriptUpdated); err != nil {
		return nil, fmt.Errorf("configuring check-route-overlap service: %w", err)
	}

	item, err := anypb.New(settings)
	if err != nil {
		return nil, err
	}
	return actions.ApplyActionResponse_builder{Item: item}.Build(), nil
}

func validateCheckRouteOverlapSpec(spec *linux.CheckRouteOverlapSpec) error {
	if spec == nil {
		return fmt.Errorf("spec is required")
	}
	switch spec.GetMode() {
	case linux.CheckRouteOverlapSpec_MODE_UNSPECIFIED,
		linux.CheckRouteOverlapSpec_WARN,
		linux.CheckRouteOverlapSpec_STRICT:
		return nil
	default:
		return fmt.Errorf(
			"invalid mode %d: must be one of MODE_UNSPECIFIED(0), WARN(1), or STRICT(2)",
			spec.GetMode(),
		)
	}
}

func (a *checkRouteOverlapAction) ensureCheckRouteOverlapUnit(ctx context.Context, scriptUpdated bool) error {
	unitUpdated, err := a.systemd.EnsureUnitFile(ctx, checkRouteOverlapUnit, checkRouteOverlapServiceUnit)
	if err != nil {
		return err
	}
	if err := a.systemd.EnableUnit(ctx, checkRouteOverlapUnit); err != nil {
		return err
	}
	changed := unitUpdated || scriptUpdated
	return systemd.EnsureUnitRunning(ctx, a.systemd, checkRouteOverlapUnit, unitUpdated, changed)
}

// renderCheckRouteOverlapScript produces a bash script that, for each
// expected CIDR, picks a probe address inside the prefix and runs
// `ip -4 route get <probe>` to ask the kernel which interface that
// address would actually go out. Any mismatch with the IPv4 default
// route's interface is logged; in STRICT mode the script then exits 1
// and (because the unit is RequiredBy=kubelet.service) kubelet does
// not start.
func renderCheckRouteOverlapScript(cidrs []string, mode linux.CheckRouteOverlapSpec_Mode) (string, error) {
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
		// Probe with first usable address (network address + 1). Works for
		// any prefix shorter than /32; for /32 we just probe the address.
		probe := prefix.Addr()
		if prefix.Bits() < 32 {
			probe = probe.Next()
		}
		entries = append(entries, entry{cidr: c, probe: probe.String()})
	}

	failExit := "0"
	modeLabel := "WARN"
	if mode == linux.CheckRouteOverlapSpec_STRICT {
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
