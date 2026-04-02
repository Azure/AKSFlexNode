package v20260301

import (
	"bytes"
	"context"
	"strings"

	"github.com/Azure/AKSFlexNode/components/kubelet"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/systemd"
)

const (
	// dropInKubeconfig is the agent-managed drop-in that sets KUBELET_KUBECONFIG_ARGS.
	// Operators can override the kubeconfig path used by kubelet by placing a
	// higher-numbered drop-in under /etc/systemd/system/kubelet.service.d/ that
	// redefines KUBELET_KUBECONFIG_ARGS. The agent will never modify drop-ins it
	// does not own.
	dropInKubeconfig = "10-flex-node-kubeconfig.conf"

	// dropInNodeConfig is the agent-managed drop-in that sets KUBELET_NODE_CONFIG_ARGS
	// and KUBELET_TUNING_ARGS (node labels, verbosity, DNS, resource reservations, etc.).
	dropInNodeConfig = "20-flex-node-node-config.conf"

	// dropInEnvFile is a static agent-managed drop-in that tells systemd to load
	// /etc/default/kubelet as an optional EnvironmentFile. The file at that path is
	// NOT managed by the agent; place it on disk to inject or override KUBELET_*
	// variables (e.g. KUBELET_KUBECONFIG_ARGS, KUBELET_EXTRA_ARGS) without touching
	// any agent-managed file. It takes precedence over the earlier 10- and 20-
	// drop-ins, and can itself be overridden by a 90-custom.conf.
	dropInEnvFile = "50-flex-node-env-file.conf"
)

func (s *startKubeletServiceAction) ensureSystemdUnit(
	ctx context.Context,
	needsRestart bool,
	spec *kubelet.StartKubeletServiceSpec,
) error {
	kubeletConfig := spec.GetKubeletConfig()

	var (
		useBootstrapKubeconfig bool
		rotateCertificates     bool
	)
	if spec.GetNodeAuthInfo().HasBootstrapTokenCredential() {
		useBootstrapKubeconfig = true
		// When bootstrap token is used, kubelet client certificate is rotated by kubelet itself
		// TODO: consider making this configurable in the spec level
		rotateCertificates = true
	}

	// Write the kubeconfig drop-in (10-flex-node-kubeconfig.conf).
	// This sets KUBELET_KUBECONFIG_ARGS so the kubeconfig path is independently
	// configurable. Operators can create a higher-numbered drop-in to redefine
	// KUBELET_KUBECONFIG_ARGS without forking the base unit file.
	kubeconfigDropIn, err := renderTemplate(dropInKubeconfig, map[string]any{
		"UseBootstrapKubeconfig":  useBootstrapKubeconfig,
		"BootstrapKubeconfigPath": config.KubeletBootstrapKubeconfigPath,
		"KubeconfigPath":          config.KubeletKubeconfigPath,
		"RotateCertificates":      rotateCertificates,
	})
	if err != nil {
		return err
	}
	kubeconfigDropInUpdated, err := s.systemd.EnsureDropInFile(ctx, config.SystemdUnitKubelet, dropInKubeconfig, kubeconfigDropIn)
	if err != nil {
		return err
	}

	// Write the node-config drop-in (20-flex-node-node-config.conf).
	// This sets KUBELET_NODE_CONFIG_ARGS and KUBELET_TUNING_ARGS from the
	// agent's resolved node configuration.
	nodeConfigDropIn, err := renderTemplate(dropInNodeConfig, map[string]any{
		"NodeLabels":           mapPairsToString(spec.GetNodeLabels(), "=", ","),
		"Verbosity":            kubeletConfig.GetVerbosity(),
		"ClientCAFile":         apiServerClientCAPath, // prepared in ensureAPIServerCA
		"ClusterDNS":           kubeletConfig.GetClusterDns(),
		"RegisterWithTaints":   strings.Join(spec.GetRegisterWithTaints(), ","),
		"EvictionHard":         mapPairsToString(kubeletConfig.GetEvictionHard(), "<", ","),
		"KubeReserved":         mapPairsToString(kubeletConfig.GetKubeReserved(), "=", ","),
		"ImageGCHighThreshold": kubeletConfig.GetImageGcHighThreshold(),
		"ImageGCLowThreshold":  kubeletConfig.GetImageGcLowThreshold(),
		"MaxPods":              kubeletConfig.GetMaxPods(),
		"NodeIP":               spec.GetNodeIp(),
	})
	if err != nil {
		return err
	}
	nodeConfigDropInUpdated, err := s.systemd.EnsureDropInFile(ctx, config.SystemdUnitKubelet, dropInNodeConfig, nodeConfigDropIn)
	if err != nil {
		return err
	}

	// Write the env-file drop-in (50-flex-node-env-file.conf).
	// This is a static file that tells systemd to load /etc/default/kubelet as an
	// optional EnvironmentFile. The agent never writes to that path; place it on
	// disk to inject or override KUBELET_* variables without touching any
	// agent-managed file.
	envFileDropIn, err := renderTemplate(dropInEnvFile, nil)
	if err != nil {
		return err
	}
	envFileDropInUpdated, err := s.systemd.EnsureDropInFile(ctx, config.SystemdUnitKubelet, dropInEnvFile, envFileDropIn)
	if err != nil {
		return err
	}

	// The base unit file is now static (no template parameters): it references
	// the env vars set by the drop-ins above.
	unitContent, err := renderTemplate("kubelet.service", nil)
	if err != nil {
		return err
	}
	unitUpdated, err := s.systemd.EnsureUnitFile(ctx, config.SystemdUnitKubelet, unitContent)
	if err != nil {
		return err
	}

	// Reload the daemon whenever any unit file or drop-in has changed so that
	// systemd picks up the new configuration before we start/restart the unit.
	anyUpdated := unitUpdated || kubeconfigDropInUpdated || nodeConfigDropInUpdated || envFileDropInUpdated
	return systemd.EnsureUnitRunning(ctx, s.systemd, config.SystemdUnitKubelet, anyUpdated, needsRestart || anyUpdated)
}

func renderTemplate(name string, params any) ([]byte, error) {
	b := &bytes.Buffer{}
	if err := assetsTemplate.ExecuteTemplate(b, name, params); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
