package localdns

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"

	"github.com/google/renameio/v2"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

const (
	environmentPath = "/etc/localdns/environment"
	criticalFQDNs   = "LOCALDNS_CRITICAL_FQDNS"
)

type configureTask struct {
	logger          *slog.Logger
	fqdn            string
	environmentPath string
}

func Configure(cfg *config.Config, logger *slog.Logger) phases.Task {
	fqdn := cfg.Node.Kubelet.ClusterFQDN
	if host, _, err := net.SplitHostPort(fqdn); err == nil {
		fqdn = host
	}
	return &configureTask{
		logger:          logger,
		fqdn:            fqdn,
		environmentPath: environmentPath,
	}
}

func (t *configureTask) Name() string { return "configure-localdns" }

func (t *configureTask) Do(context.Context) error {
	data, err := os.ReadFile(t.environmentPath)
	if errors.Is(err, os.ErrNotExist) {
		t.logger.Debug("local DNS configuration not found", "path", t.environmentPath)
		return nil
	}
	if err != nil {
		return fmt.Errorf("read local DNS environment: %w", err)
	}

	updated, changed := addCriticalFQDN(string(data), t.fqdn)
	if !changed {
		return nil
	}

	info, err := os.Stat(t.environmentPath)
	if err != nil {
		return fmt.Errorf("stat local DNS environment: %w", err)
	}
	if err := renameio.WriteFile(t.environmentPath, []byte(updated), info.Mode().Perm()); err != nil {
		return fmt.Errorf("write local DNS environment: %w", err)
	}
	t.logger.Info("added AKS API server to local DNS critical FQDNs", "fqdn", t.fqdn)
	return nil
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
