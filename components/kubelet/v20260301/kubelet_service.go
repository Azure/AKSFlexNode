package v20260301

import (
	"bytes"
	"context"

	"github.com/Azure/AKSFlexNode/components/kubelet"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/systemd"
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

	params := map[string]any{
		"NodeLabels":              mapPairsToString(spec.GetNodeLabels(), "=", ","),
		"Verbosity":               kubeletConfig.GetVerbosity(),
		"ClientCAFile":            apiServerClientCAPath, // prepared in ensureAPIServerCA
		"ClusterDNS":              kubeletConfig.GetClusterDns(),
		"EvictionHard":            mapPairsToString(kubeletConfig.GetEvictionHard(), "<", ","),
		"KubeReserved":            mapPairsToString(kubeletConfig.GetKubeReserved(), "=", ","),
		"ImageGCHighThreshold":    kubeletConfig.GetImageGcHighThreshold(),
		"ImageGCLowThreshold":     kubeletConfig.GetImageGcLowThreshold(),
		"MaxPods":                 kubeletConfig.GetMaxPods(),
		"RotateCertificates":      rotateCertificates,
		"UseBootstrapKubeconfig":  useBootstrapKubeconfig,
		"BootstrapKubeconfigPath": config.KubeletBootstrapKubeconfigPath,
		"KubeconfigPath":          config.KubeletKubeconfigPath,
	}

	b := &bytes.Buffer{}
	if err := assetsTemplate.ExecuteTemplate(b, "kubelet.service", params); err != nil {
		return err
	}

	unitUpdated, err := s.systemd.EnsureUnitFile(ctx, config.SystemdUnitKubelet, b.Bytes())
	if err != nil {
		return err
	}

	return systemd.EnsureUnitRunning(ctx, s.systemd, config.SystemdUnitKubelet, unitUpdated, needsRestart || unitUpdated)
}
