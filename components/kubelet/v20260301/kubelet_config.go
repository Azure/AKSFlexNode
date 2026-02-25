package v20260301

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/clientcmd/api/latest"

	"go.goms.io/aks/AKSFlexNode/components/kubelet"
	"go.goms.io/aks/AKSFlexNode/pkg/utils/utilio"
)

func mapPairsToString(pairs map[string]string, kvSep, pairSep string) string {
	xs := make([]string, 0, len(pairs))
	for k, v := range pairs {
		xs = append(xs, fmt.Sprintf("%s%s%s", k, kvSep, v))
	}
	sort.Strings(xs)
	return strings.Join(xs, pairSep)
}

func fileHasIdenticalContent(filePath string, desiredContent []byte) (bool, error) {
	actualContent, err := os.ReadFile(filePath) //#nosec - file path has been validated by caller
	switch {
	case os.IsNotExist(err):
		// File does not exist, so it does not have identical content
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read %q: %w", filePath, err)
	default:
		return bytes.Equal(desiredContent, actualContent), nil
	}
}

func (s *startKubeletServiceAction) ensureKubeletConfig(
	spec *kubelet.StartKubeletServiceSpec,
) (bool, error) {
	apiServerCAChanged, err := s.ensureAPIServerCA(spec)
	if err != nil {
		return false, err
	}

	kubeletEnvChanged, err := s.ensureKubeletEnvFile(spec)
	if err != nil {
		return false, err
	}

	bootstrapKubeConfigChanged, err := s.ensureBootstrapKubeconfig(spec)
	if err != nil {
		return false, err
	}

	kubeletKubeconfigChanged, err := s.ensureKubeletKubeconfig(spec)
	if err != nil {
		return false, err
	}

	configsChanged := apiServerCAChanged ||
		kubeletEnvChanged ||
		bootstrapKubeConfigChanged ||
		kubeletKubeconfigChanged
	return configsChanged, nil
}

func (s *startKubeletServiceAction) ensureAPIServerCA(
	spec *kubelet.StartKubeletServiceSpec,
) (bool, error) {
	desiredContent := spec.GetControlPlane().GetCertificateAuthorityData()
	if idential, err := fileHasIdenticalContent(apiServerClientCAPath, desiredContent); err != nil {
		return false, err
	} else if idential {
		return false, nil
	}

	// FIXME: consider using 0640?
	if err := utilio.WriteFile(apiServerClientCAPath, desiredContent, 0644); err != nil {
		return false, fmt.Errorf("write %q: %w", apiServerClientCAPath, err)
	}
	return true, nil
}

func (s *startKubeletServiceAction) ensureKubeletEnvFile(
	spec *kubelet.StartKubeletServiceSpec,
) (bool, error) {
	kubeletConfig := spec.GetKubeletConfig()

	rotateCertificates := false
	if spec.GetNodeAuthInfo().HasBootstrapTokenCredential() {
		// When bootstrap token is used, kubelet client certificate is rotated by kubelet itself
		// TODO: consider making this configurable in the spec level
		rotateCertificates = true
	}

	// FIXME: consider migrate using kubelet config file instead of env file
	b := &bytes.Buffer{}
	if err := assetsTemplate.ExecuteTemplate(b, "kubelet.env", map[string]any{
		"NodeLabels":           mapPairsToString(spec.GetNodeLabels(), "=", ","),
		"Verbosity":            kubeletConfig.GetVerbosity(),
		"ClientCAFile":         apiServerClientCAPath, // prepared in ensureAPIServerCA
		"ClusterDNS":           kubeletConfig.GetClusterDns(),
		"EvictionHard":         mapPairsToString(kubeletConfig.GetEvictionHard(), "<", ","),
		"KubeReserved":         mapPairsToString(kubeletConfig.GetKubeReserved(), "=", ","),
		"ImageGCHighThreshold": kubeletConfig.GetImageGcHighThreshold(),
		"ImageGCLowThreshold":  kubeletConfig.GetImageGcLowThreshold(),
		"MaxPods":              kubeletConfig.GetMaxPods(),
		"RotateCertificates":   rotateCertificates,
	}); err != nil {
		return false, err
	}

	desiredContent := b.Bytes()
	if idential, err := fileHasIdenticalContent(envFileKubelet, desiredContent); err != nil {
		return false, err
	} else if idential {
		return false, nil
	}

	// FIXME: consider using 0640?
	if err := utilio.WriteFile(envFileKubelet, desiredContent, 0644); err != nil {
		return false, fmt.Errorf("write %q: %w", envFileKubelet, err)
	}
	return true, nil
}

func (s *startKubeletServiceAction) ensureKubeletKubeconfig(
	spec *kubelet.StartKubeletServiceSpec,
) (bool, error) {
	nodeAuthInfo := spec.GetNodeAuthInfo()
	if nodeAuthInfo.HasBootstrapTokenCredential() {
		return false, nil // kubelet kubeconfig is not set when bootstrap token is used
	}

	selfBinary, err := os.Executable() // TODO: allow overriding the path in config spec
	if err != nil {
		return false, fmt.Errorf("get self executable path: %w", err)
	}

	// refs:
	// - https://github.com/Azure/kubelogin/blob/main/pkg/internal/token/options.go
	authInfoSettings := &api.AuthInfo{
		Exec: &api.ExecConfig{
			APIVersion: "client.authentication.k8s.io/v1",
			Command:    selfBinary,
			Args: []string{
				"token", "kubelogin",
			},
		},
	}
	switch {
	case nodeAuthInfo.HasArcCredential():
		// TODO: implement arc credential support with pop
		return false, fmt.Errorf("arc credential is not supported yet")
	case nodeAuthInfo.HasServicePrincipalCredential():
		cred := nodeAuthInfo.GetServicePrincipalCredential()
		authInfoSettings.Exec.Env = append(
			authInfoSettings.Exec.Env,
			api.ExecEnvVar{
				Name:  "AAD_LOGIN_METHOD",
				Value: "spn",
			},
			api.ExecEnvVar{
				Name:  "AZURE_CLIENT_ID",
				Value: cred.GetClientId(),
			},
			api.ExecEnvVar{
				Name:  "AZURE_TENANT_ID",
				Value: cred.GetTenantId(),
			},
			api.ExecEnvVar{
				Name:  "AZURE_CLIENT_SECRET",
				Value: cred.GetClientSecret(),
			},
		)
	case nodeAuthInfo.HasMsiCredential():
		cred := nodeAuthInfo.GetMsiCredential()
		authInfoSettings.Exec.Env = append(
			authInfoSettings.Exec.Env,
			api.ExecEnvVar{
				Name:  "AAD_LOGIN_METHOD",
				Value: "msi",
			},
			api.ExecEnvVar{
				Name:  "AZURE_CLIENT_ID",
				Value: cred.GetClientId(),
			},
			api.ExecEnvVar{
				Name:  "AZURE_TENANT_ID",
				Value: cred.GetTenantId(),
			},
		)
	default:
		return false, fmt.Errorf("unsupported node auth info type")
	}
	k := kubeletKubeConfig(spec.GetControlPlane(), authInfoSettings)
	desiredContent, err := runtime.Encode(latest.Codec, k)
	if err != nil {
		return false, err
	}

	if idential, err := fileHasIdenticalContent(kubeletKubeconfigPath, desiredContent); err != nil {
		return false, err
	} else if idential {
		return false, nil
	}

	// FIXME: consider using 0640?
	if err := utilio.WriteFile(kubeletKubeconfigPath, desiredContent, 0644); err != nil {
		return false, fmt.Errorf("write %q: %w", kubeletKubeconfigPath, err)
	}
	return true, nil
}

func (s *startKubeletServiceAction) ensureBootstrapKubeconfig(
	spec *kubelet.StartKubeletServiceSpec,
) (bool, error) {
	// NOTE: bootstrap kubconfig is used only when bootstrap token is set
	if !spec.GetNodeAuthInfo().HasBootstrapTokenCredential() {
		return false, nil
	}

	authInfoSettings := &api.AuthInfo{
		Token: spec.GetNodeAuthInfo().GetBootstrapTokenCredential().GetToken(),
	}
	k := kubeletKubeConfig(spec.GetControlPlane(), authInfoSettings)
	desiredContent, err := runtime.Encode(latest.Codec, k)
	if err != nil {
		return false, err
	}

	if idential, err := fileHasIdenticalContent(bootstrapKubeconfigPath, desiredContent); err != nil {
		return false, err
	} else if idential {
		return false, nil
	}

	// FIXME: consider using 0640?
	if err := utilio.WriteFile(bootstrapKubeconfigPath, desiredContent, 0644); err != nil {
		return false, fmt.Errorf("write %q: %w", bootstrapKubeconfigPath, err)
	}
	return true, nil
}

func kubeletKubeConfig(
	controlPlane *kubelet.ControlPlane,
	authInfoSettings *api.AuthInfo,
) *api.Config {
	const (
		cluster  = "cluster"
		context  = "context"
		authInfo = "user"
	)

	return &api.Config{
		Kind: "Config",
		Clusters: map[string]*api.Cluster{
			cluster: {
				Server:                   controlPlane.GetServer(),
				CertificateAuthorityData: controlPlane.GetCertificateAuthorityData(),
			},
		},
		CurrentContext: context,
		Contexts: map[string]*api.Context{
			context: {
				Cluster:  cluster,
				AuthInfo: authInfo,
			},
		},
		AuthInfos: map[string]*api.AuthInfo{
			authInfo: authInfoSettings,
		},
	}
}
