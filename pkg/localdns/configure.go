package localdns

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/google/renameio/v2"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

const (
	environmentPath       = "/etc/localdns/environment"
	hostsPath             = "/etc/localdns/hosts"
	hostsSetupScriptPath  = "/opt/azure/containers/aks-localdns-hosts-setup.sh"
	hostsSetupServicePath = "/etc/systemd/system/aks-localdns-hosts-setup.service"
	hostsSetupTimerPath   = "/etc/systemd/system/aks-localdns-hosts-setup.timer"
	localdnsServicePath   = "/etc/systemd/system/localdns.service"
	localdnsScriptPath    = "/opt/azure/containers/localdns/localdns.sh"
	activeCorefilePath    = "/opt/azure/containers/localdns/updated.localdns.corefile"

	criticalFQDNs         = "LOCALDNS_CRITICAL_FQDNS"
	corefileBase          = "LOCALDNS_COREFILE_BASE"
	corefileWithHosts     = "LOCALDNS_COREFILE_WITH_HOSTS"
	shouldEnableHosts     = "SHOULD_ENABLE_HOSTS_PLUGIN"
	hostsSetupServiceUnit = "aks-localdns-hosts-setup.service"
	hostsSetupTimerUnit   = "aks-localdns-hosts-setup.timer"
	localdnsServiceUnit   = "localdns.service"
)

type configureTask struct {
	logger                *slog.Logger
	fqdn                  string
	environmentPath       string
	hostsPath             string
	hostsSetupScriptPath  string
	hostsSetupServicePath string
	hostsSetupTimerPath   string
	localdnsServicePath   string
	localdnsScriptPath    string
	activeCorefilePath    string
	runSystemctl          func(context.Context, ...string) error
}

func Configure(cfg *config.Config, logger *slog.Logger) phases.Task {
	return &configureTask{
		logger:                logger,
		fqdn:                  fqdnHost(cfg.Node.Kubelet.ClusterFQDN),
		environmentPath:       environmentPath,
		hostsPath:             hostsPath,
		hostsSetupScriptPath:  hostsSetupScriptPath,
		hostsSetupServicePath: hostsSetupServicePath,
		hostsSetupTimerPath:   hostsSetupTimerPath,
		localdnsServicePath:   localdnsServicePath,
		localdnsScriptPath:    localdnsScriptPath,
		activeCorefilePath:    activeCorefilePath,
		runSystemctl: func(ctx context.Context, args ...string) error {
			return utilexec.RunCmd(ctx, logger, utilexec.Systemctl(), args...)
		},
	}
}

func (t *configureTask) Name() string { return "configure-localdns" }

func (t *configureTask) Do(ctx context.Context) error {
	data, err := os.ReadFile(t.environmentPath)
	if errors.Is(err, os.ErrNotExist) {
		t.logger.Debug("local DNS configuration not found", "path", t.environmentPath)
		return nil
	}
	if err != nil {
		return fmt.Errorf("read local DNS environment: %w", err)
	}

	if t.fqdn == "" {
		return nil
	}
	if !hasEnvironmentValue(string(data), corefileBase) ||
		!hasEnvironmentValue(string(data), corefileWithHosts) ||
		!t.hasRequiredArtifacts() {
		t.logger.Debug("local DNS hosts plugin is not available")
		return nil
	}

	updated, changed := addCriticalFQDN(string(data), t.fqdn)
	updated, enabled := setEnvironmentValue(updated, shouldEnableHosts, "true")
	if changed || enabled {
		info, err := os.Stat(t.environmentPath)
		if err != nil {
			return fmt.Errorf("stat local DNS environment: %w", err)
		}
		if err := renameio.WriteFile(t.environmentPath, []byte(updated), info.Mode().Perm()); err != nil {
			return fmt.Errorf("write local DNS environment: %w", err)
		}
	}

	hosts, err := os.OpenFile(t.hostsPath, os.O_WRONLY|os.O_CREATE, 0o644) //nolint:gosec // AgentBaker CoreDNS reads this hosts file.
	if err != nil {
		return fmt.Errorf("create local DNS hosts file: %w", err)
	}
	if err := hosts.Close(); err != nil {
		return fmt.Errorf("close local DNS hosts file: %w", err)
	}

	if err := t.runSystemctl(ctx, "start", hostsSetupServiceUnit); err != nil {
		return fmt.Errorf("populate local DNS hosts file: %w", err)
	}
	if err := t.runSystemctl(ctx, "enable", "--now", hostsSetupTimerUnit); err != nil {
		return fmt.Errorf("enable local DNS hosts refresh: %w", err)
	}
	if !t.activeCorefileHasHosts() {
		if err := t.runSystemctl(ctx, "restart", localdnsServiceUnit); err != nil {
			return fmt.Errorf("restart local DNS with hosts plugin: %w", err)
		}
	}
	t.logger.Info("added AKS API server to local DNS critical FQDNs", "fqdn", t.fqdn)
	return nil
}

func (t *configureTask) hasRequiredArtifacts() bool {
	for _, artifact := range []struct {
		path       string
		executable bool
	}{
		{t.hostsSetupScriptPath, true},
		{t.hostsSetupServicePath, false},
		{t.hostsSetupTimerPath, false},
		{t.localdnsServicePath, false},
		{t.localdnsScriptPath, true},
	} {
		info, err := os.Stat(artifact.path)
		if err != nil || !info.Mode().IsRegular() || artifact.executable && info.Mode().Perm()&0o111 == 0 {
			return false
		}
	}
	return true
}

func (t *configureTask) activeCorefileHasHosts() bool {
	data, err := os.ReadFile(t.activeCorefilePath)
	return err == nil && strings.Contains(string(data), "hosts /etc/localdns/hosts")
}

func hasEnvironmentValue(environment, name string) bool {
	prefix := name + "="
	for _, line := range strings.Split(environment, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix)) != ""
		}
	}
	return false
}

func setEnvironmentValue(environment, name, value string) (string, bool) {
	lines := strings.Split(environment, "\n")
	prefix := name + "="
	for i, line := range lines {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		if line == prefix+value {
			return environment, false
		}
		lines[i] = prefix + value
		return strings.Join(lines, "\n"), true
	}

	suffix := ""
	if environment != "" && !strings.HasSuffix(environment, "\n") {
		suffix = "\n"
	}
	return environment + suffix + prefix + value + "\n", true
}

func addCriticalFQDN(environment, fqdn string) (string, bool) {
	if fqdn == "" {
		return environment, false
	}

	lines := strings.Split(environment, "\n")
	prefix := criticalFQDNs + "="
	for i, line := range lines {
		if !strings.HasPrefix(line, prefix) {
			continue
		}

		for _, existing := range strings.Split(strings.TrimPrefix(line, prefix), ",") {
			if strings.TrimSpace(existing) == fqdn {
				return environment, false
			}
		}
		if line == prefix {
			lines[i] += fqdn
		} else {
			lines[i] += "," + fqdn
		}
		return strings.Join(lines, "\n"), true
	}

	suffix := ""
	if environment != "" && !strings.HasSuffix(environment, "\n") {
		suffix = "\n"
	}
	return environment + suffix + prefix + fqdn + "\n", true
}

func fqdnHost(value string) string {
	value = strings.TrimSpace(value)
	if parsed, err := url.Parse(value); err == nil && parsed.Scheme != "" && parsed.Hostname() != "" {
		return parsed.Hostname()
	}
	if host, _, err := net.SplitHostPort(value); err == nil {
		return host
	}
	return value
}
