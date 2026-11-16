package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

const nspawnDependenciesDropIn = "aks-flex-node-dependencies.conf"

type configureNSpawnDependenciesTask struct {
	log               *slog.Logger
	serviceDropInPath string
	requiredServices  []string
	reload            func(context.Context, *slog.Logger) error
}

// ConfigureNSpawnDependencies orders the machine after host services that
// must finish changing host devices before nspawn starts.
func ConfigureNSpawnDependencies(log *slog.Logger, rootFS *goalstates.RootFS, requiredServices []string) phases.Task {
	return &configureNSpawnDependenciesTask{
		log:               log,
		serviceDropInPath: filepath.Join(filepath.Dir(rootFS.ServiceOverrideFile), nspawnDependenciesDropIn),
		requiredServices:  requiredServices,
		reload:            utilexec.ReloadSystemd,
	}
}

func (t *configureNSpawnDependenciesTask) Name() string {
	return "configure-nspawn-systemd-dependencies"
}

func (t *configureNSpawnDependenciesTask) Do(ctx context.Context) error {
	var content bytes.Buffer
	content.WriteString("[Unit]\n")
	content.WriteString("Wants=systemd-udev-settle.service\n")
	content.WriteString("After=systemd-udev-settle.service\n")
	if len(t.requiredServices) > 0 {
		services := strings.Join(t.requiredServices, " ")
		fmt.Fprintf(&content, "Requires=%s\n", services)
		fmt.Fprintf(&content, "After=%s\n", services)
	}

	if err := utilio.WriteFile(t.serviceDropInPath, content.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write nspawn dependency drop-in %s: %w", t.serviceDropInPath, err)
	}
	if err := t.reload(ctx, t.log); err != nil {
		return fmt.Errorf("reload systemd after configuring nspawn dependencies: %w", err)
	}
	return nil
}
