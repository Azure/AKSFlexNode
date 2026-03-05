package v20260301

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"google.golang.org/protobuf/types/known/anypb"

	"github.com/Azure/AKSFlexNode/components/linux"
	"github.com/Azure/AKSFlexNode/components/services/actions"
	"github.com/Azure/AKSFlexNode/pkg/systemd"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilpb"
)

const (
	dockerServiceUnit = "docker.service"
	dockerSocketUnit  = "docker.socket"
	daemonConfigPath  = "/etc/docker/daemon.json"
)

// disableDockerAction disables the docker service and configures the docker
// daemon with "iptables": false. This prevents docker from manipulating
// iptables rules, which would conflict with Kubernetes networking.
type disableDockerAction struct {
	systemd systemd.Manager
}

func newDisableDockerAction() (actions.Server, error) {
	return &disableDockerAction{
		systemd: systemd.New(),
	}, nil
}

var _ actions.Server = (*disableDockerAction)(nil)

func (a *disableDockerAction) ApplyAction(
	ctx context.Context,
	req *actions.ApplyActionRequest,
) (*actions.ApplyActionResponse, error) {
	settings, err := utilpb.AnyTo[*linux.DisableDocker](req.GetItem())
	if err != nil {
		return nil, err
	}

	if err := a.ensureDockerDisabled(ctx); err != nil {
		return nil, fmt.Errorf("disabling docker: %w", err)
	}

	if err := a.ensureDaemonConfig(); err != nil {
		return nil, fmt.Errorf("configuring docker daemon: %w", err)
	}

	item, err := anypb.New(settings)
	if err != nil {
		return nil, err
	}

	return actions.ApplyActionResponse_builder{Item: item}.Build(), nil
}

// ensureDockerDisabled idempotently stops, disables, and masks the docker
// service and socket units so docker cannot be started.
func (a *disableDockerAction) ensureDockerDisabled(ctx context.Context) error {
	if err := systemd.EnsureUnitMasked(ctx, a.systemd, dockerSocketUnit); err != nil {
		return fmt.Errorf("masking %s: %w", dockerSocketUnit, err)
	}

	if err := systemd.EnsureUnitMasked(ctx, a.systemd, dockerServiceUnit); err != nil {
		return fmt.Errorf("masking %s: %w", dockerServiceUnit, err)
	}

	return nil
}

// ensureDaemonConfig ensures /etc/docker/daemon.json contains
// "iptables": false.
func (a *disableDockerAction) ensureDaemonConfig() error {
	return a.ensureDaemonConfigAt(daemonConfigPath)
}

// ensureDaemonConfigAt ensures the daemon.json at the given path
// contains "iptables": false. If the file already exists, the existing
// configuration is preserved and only the iptables key is set. If the file does
// not exist, a new one is created.
func (a *disableDockerAction) ensureDaemonConfigAt(path string) error {
	config := map[string]any{}

	existing, err := os.ReadFile(path) //#nosec G304 -- trusted path
	switch {
	case errors.Is(err, os.ErrNotExist):
		// file does not exist, will create with defaults
	case err != nil:
		return fmt.Errorf("reading %s: %w", path, err)
	default:
		if err := json.Unmarshal(existing, &config); err != nil {
			return fmt.Errorf("parsing %s: %w", path, err)
		}
	}

	if val, ok := config["iptables"]; ok {
		if enabled, isBool := val.(bool); isBool && !enabled {
			// iptables is already set to false, nothing to do
			return nil
		}
	}

	config["iptables"] = false

	content, err := marshalDaemonConfig(config)
	if err != nil {
		return err
	}

	return utilio.WriteFile(path, content, 0644)
}

// marshalDaemonConfig serializes a daemon config map to indented JSON with a
// trailing newline, matching the conventional format for daemon.json.
func marshalDaemonConfig(config map[string]any) ([]byte, error) {
	data, err := json.MarshalIndent(config, "", "    ")
	if err != nil {
		return nil, fmt.Errorf("marshaling daemon config: %w", err)
	}
	data = append(data, '\n')
	return data, nil
}
