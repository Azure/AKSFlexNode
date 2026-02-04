package privatecluster

import (
	"context"
	"fmt"
	"os"

	"github.com/sirupsen/logrus"

	"go.goms.io/aks/AKSFlexNode/pkg/utils"
	"go.goms.io/aks/AKSFlexNode/pkg/utils/utilhost"
)

const (
	kubeloginVersion    = "0.1.6"
	kubeloginURLPattern = "https://github.com/Azure/kubelogin/releases/download/v%s/kubelogin-linux-%s.zip"
	kubectlURLPattern   = "https://acs-mirror.azureedge.net/kubernetes/v%s/bin/linux/%s/kubectl"
)

// ToolInstaller handles installation of CLI tools via direct downloads.
type ToolInstaller struct {
	logger *logrus.Logger
}

// NewToolInstaller creates a new ToolInstaller instance.
func NewToolInstaller(logger *logrus.Logger) *ToolInstaller {
	return &ToolInstaller{logger: logger}
}

// InstallKubelogin downloads and installs kubelogin binary.
func (t *ToolInstaller) InstallKubelogin(ctx context.Context) error {
	if CommandExists("kubelogin") {
		return nil
	}

	arch := utilhost.GetArch()
	url := fmt.Sprintf(kubeloginURLPattern, kubeloginVersion, arch)
	zipPath := "/tmp/kubelogin.zip"

	t.logger.Infof("Downloading kubelogin v%s...", kubeloginVersion)

	if _, err := utils.RunCommandWithOutputContext(ctx, "curl", "-L", "-o", zipPath, url); err != nil {
		return fmt.Errorf("failed to download kubelogin: %w", err)
	}
	defer func() { _ = os.Remove(zipPath) }()

	extractDir := "/tmp/kubelogin-extract"
	_ = os.RemoveAll(extractDir)
	if _, err := utils.RunCommandWithOutputContext(ctx, "unzip", "-o", zipPath, "-d", extractDir); err != nil {
		return fmt.Errorf("failed to extract kubelogin: %w", err)
	}
	defer func() { _ = os.RemoveAll(extractDir) }()

	binaryPath := fmt.Sprintf("%s/bin/linux_%s/kubelogin", extractDir, arch)
	if !utils.FileExists(binaryPath) {
		return fmt.Errorf("kubelogin binary not found at %s", binaryPath)
	}

	if _, err := utils.RunCommandWithOutputContext(ctx, "cp", binaryPath, "/usr/local/bin/kubelogin"); err != nil {
		return fmt.Errorf("failed to install kubelogin: %w", err)
	}
	_ = os.Chmod("/usr/local/bin/kubelogin", 0755) // #nosec G302 G306 -- binary must be executable

	t.logger.Infof("kubelogin v%s installed", kubeloginVersion)
	return nil
}

// InstallKubectl downloads and installs kubectl binary.
func (t *ToolInstaller) InstallKubectl(ctx context.Context, kubernetesVersion string) error {
	if CommandExists("kubectl") {
		return nil
	}

	arch := utilhost.GetArch()
	url := fmt.Sprintf(kubectlURLPattern, kubernetesVersion, arch)

	t.logger.Infof("Downloading kubectl v%s...", kubernetesVersion)

	if _, err := utils.RunCommandWithOutputContext(ctx, "curl", "-L", "-o", "/usr/local/bin/kubectl", url); err != nil {
		return fmt.Errorf("failed to download kubectl: %w", err)
	}
	_ = os.Chmod("/usr/local/bin/kubectl", 0755) // #nosec G302 G306 -- binary must be executable

	t.logger.Infof("kubectl v%s installed", kubernetesVersion)
	return nil
}
