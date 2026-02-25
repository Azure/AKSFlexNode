package v20260301

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"go.goms.io/aks/AKSFlexNode/components/kubelet"
	"go.goms.io/aks/AKSFlexNode/pkg/systemd"
)

func (s *startKubeletServiceAction) ensureSystemdUnit(
	ctx context.Context,
	needsRestart bool,
	spec *kubelet.StartKubeletServiceSpec,
) error {
	_, err := s.systemd.GetUnitStatus(ctx, systemdUnitKubelet)
	switch {
	case errors.Is(err, systemd.ErrUnitNotFound):
		return s.createSystemdUnit(ctx, spec)
	case err != nil:
		return err
	default:
		return s.updateSystemdUnit(ctx, needsRestart)
	}
}

func (s *startKubeletServiceAction) createSystemdUnit(
	ctx context.Context,
	spec *kubelet.StartKubeletServiceSpec,
) error {
	// TODO: consider unifying the drop-in / environment file usages
	// We don't actually need extra drop-in files for configuring containerd / tls bootstrapping flags
	// as they are supported since day 1 in flex node.
	systemdDropInFiles := []string{
		"10-containerd.conf",
	}
	if spec.GetNodeAuthInfo().HasBootstrapTokenCredential() {
		systemdDropInFiles = append(systemdDropInFiles, "10-tlsbootstrap.conf")
	}

	for _, fileName := range systemdDropInFiles {
		dropInContent, err := assets.ReadFile(filepath.Join("assets", fileName))
		if err != nil {
			return fmt.Errorf("read systemd drop-in file %s: %w", fileName, err)
		}
		if err := s.systemd.WriteDropInFile(
			ctx,
			systemdUnitKubelet,
			fileName, dropInContent,
		); err != nil {
			return fmt.Errorf("write systemd drop-in file %s: %w", fileName, err)
		}
	}

	b := &bytes.Buffer{}
	if err := assetsTemplate.ExecuteTemplate(b, "kubelet.service", map[string]any{
		"EnvFilePath": envFileKubelet, // prepared in ensureKubeletConfig
	}); err != nil {
		return err
	}

	if err := s.systemd.WriteUnitFile(ctx, systemdUnitKubelet, b.Bytes()); err != nil {
		return err
	}

	if err := s.systemd.DaemonReload(ctx); err != nil {
		return err
	}

	if err := s.systemd.StartUnit(ctx, systemdUnitKubelet); err != nil {
		return err
	}

	return nil
}

func (s *startKubeletServiceAction) updateSystemdUnit(ctx context.Context, restart bool) error {
	// TODO: should we allow updating kubelet.service?

	if restart {
		if err := s.systemd.ReloadOrRestartUnit(ctx, systemdUnitKubelet); err != nil {
			return err
		}
	}

	return nil
}
