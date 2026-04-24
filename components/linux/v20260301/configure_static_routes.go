package v20260301

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"text/template"

	"google.golang.org/protobuf/types/known/anypb"

	"github.com/Azure/AKSFlexNode/components/linux"
	"github.com/Azure/AKSFlexNode/components/services/actions"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/systemd"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilpb"
)

const (
	staticRoutesUnit       = "static-routes.service"
	staticRoutesScriptPath = config.ConfigDir + "/static-routes.sh"
)

//go:embed assets/static-routes.service
var staticRoutesServiceUnit []byte

//go:embed assets/static-routes.sh.tpl
var staticRoutesScriptTemplate string

// configureStaticRoutesAction installs a oneshot systemd unit that applies
// one or more static IPv4 routes via `ip route replace` before kubelet
// starts. Intended for cases where the VM provider's default routing is
// wrong for the cluster — for example, Azure ND-isr SKUs install connected
// /16 routes for the InfiniBand fabric that can shadow legitimate cluster
// CIDRs. More-specific routes added via this action win over the IB /16
// without disturbing peer-to-peer IB traffic.
type configureStaticRoutesAction struct {
	systemd systemd.Manager
}

func newConfigureStaticRoutesAction() (actions.Server, error) {
	return &configureStaticRoutesAction{
		systemd: systemd.New(),
	}, nil
}

var _ actions.Server = (*configureStaticRoutesAction)(nil)

func (a *configureStaticRoutesAction) ApplyAction(
	ctx context.Context,
	req *actions.ApplyActionRequest,
) (*actions.ApplyActionResponse, error) {
	settings, err := utilpb.AnyTo[*linux.ConfigureStaticRoutes](req.GetItem())
	if err != nil {
		return nil, err
	}

	spec := settings.GetSpec()
	if err := validateConfigureStaticRoutesSpec(spec); err != nil {
		return nil, fmt.Errorf("validating ConfigureStaticRoutes spec: %w", err)
	}

	routes := spec.GetRoutes()
	if len(routes) == 0 {
		if err := systemd.EnsureUnitStoppedAndDisabled(ctx, a.systemd, staticRoutesUnit); err != nil {
			return nil, fmt.Errorf("disabling static-routes service: %w", err)
		}
		item, err := anypb.New(settings)
		if err != nil {
			return nil, err
		}
		return actions.ApplyActionResponse_builder{Item: item}.Build(), nil
	}

	script, err := renderStaticRoutesScript(routes)
	if err != nil {
		return nil, fmt.Errorf("rendering static-routes script: %w", err)
	}

	scriptUpdated, err := writeScriptIfChanged(staticRoutesScriptPath, []byte(script))
	if err != nil {
		return nil, fmt.Errorf("writing static-routes script: %w", err)
	}

	if err := a.ensureStaticRoutesUnit(ctx, scriptUpdated); err != nil {
		return nil, fmt.Errorf("configuring static-routes service: %w", err)
	}

	item, err := anypb.New(settings)
	if err != nil {
		return nil, err
	}
	return actions.ApplyActionResponse_builder{Item: item}.Build(), nil
}

func validateConfigureStaticRoutesSpec(spec *linux.ConfigureStaticRoutesSpec) error {
	if spec == nil {
		return fmt.Errorf("spec is required")
	}
	if len(spec.GetRoutes()) > 0 && !spec.GetEnabled() {
		return fmt.Errorf("routes were provided but spec.enabled=false; set spec.enabled=true to apply static routes")
	}
	return nil
}

// ensureStaticRoutesUnit installs, enables, and (re)starts the
// static-routes.service oneshot unit. The unit runs the script at
// /etc/aks-flex-node/static-routes.sh before kubelet.service starts.
// The service is restarted whenever the unit file or the script content
// has changed — the latter matters because RemainAfterExit=yes means an
// active oneshot will not rerun by itself.
func (a *configureStaticRoutesAction) ensureStaticRoutesUnit(ctx context.Context, scriptUpdated bool) error {
	unitUpdated, err := a.systemd.EnsureUnitFile(ctx, staticRoutesUnit, staticRoutesServiceUnit)
	if err != nil {
		return err
	}
	if err := a.systemd.EnableUnit(ctx, staticRoutesUnit); err != nil {
		return err
	}
	changed := unitUpdated || scriptUpdated
	return systemd.EnsureUnitRunning(ctx, a.systemd, staticRoutesUnit, unitUpdated, changed)
}

// writeScriptIfChanged writes content to path only when the existing file
// differs. Returns true when the file was created or updated. Compares
// byte-for-byte (no whitespace trimming) because the script is entirely
// machine-generated.
func writeScriptIfChanged(path string, content []byte) (bool, error) {
	existing, err := os.ReadFile(path) //#nosec G304 -- trusted path constructed from constant
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

// renderStaticRoutesScript produces an idempotent bash script that applies
// each route via `ip -4 route replace`. When `dev` is empty the script
// resolves the outbound interface from the default IPv4 route at boot time.
// When `gateway` is empty the script resolves the default gateway on that
// dev. Both lookups retry briefly to survive DHCP races.
func renderStaticRoutesScript(routes []*linux.StaticRoute) (string, error) {
	type entry struct {
		dest   string
		dev    string
		gw     string
		metric uint32
	}
	const (
		autoDevToken = "@@AUTO_DEV@@"
		autoGWToken  = "@@AUTO_GW@@"
	)
	entries := make([]entry, 0, len(routes))
	for i, r := range routes {
		dest := r.GetDestination()
		prefix, err := netip.ParsePrefix(dest)
		if err != nil {
			return "", fmt.Errorf("route %d: invalid destination %q: %w", i, dest, err)
		}
		if !prefix.Addr().Is4() {
			return "", fmt.Errorf("route %d: destination %q is not IPv4", i, dest)
		}
		if gw := r.GetGateway(); gw != "" {
			gwAddr, err := netip.ParseAddr(gw)
			if err != nil {
				return "", fmt.Errorf("route %d: invalid gateway %q: %w", i, gw, err)
			}
			if !gwAddr.Is4() {
				return "", fmt.Errorf("route %d: gateway %q is not IPv4", i, gw)
			}
		}
		if dev := r.GetDev(); dev != "" && !isSafeIfaceName(dev) {
			return "", fmt.Errorf("route %d: invalid dev name %q", i, dev)
		}
		dev := r.GetDev()
		if dev == "" {
			dev = autoDevToken
		}
		gw := r.GetGateway()
		if gw == "" {
			gw = autoGWToken
		}
		entries = append(entries, entry{
			dest:   dest,
			dev:    dev,
			gw:     gw,
			metric: r.GetMetric(),
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
// safe against malformed spec inputs. Linux interface names are <=15 chars
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
