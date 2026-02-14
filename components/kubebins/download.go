package kubebins

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"

	v20260301 "go.goms.io/aks/AKSFlexNode/components/kubebins/v20260301"
	"go.goms.io/aks/AKSFlexNode/components/services/actions"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
	"go.goms.io/aks/AKSFlexNode/pkg/utils/utilio"
	"go.goms.io/aks/AKSFlexNode/pkg/utils/utilpb"
)

const (
	// Binary installation directory
	binDir = "/usr/local/bin"

	// Kubernetes binaries
	kubeletBinary = "kubelet"
	kubectlBinary = "kubectl"
	kubeadmBinary = "kubeadm"

	// Kubernetes binary paths
	kubeletPath = binDir + "/" + kubeletBinary
	kubectlPath = binDir + "/" + kubectlBinary
	kubeadmPath = binDir + "/" + kubeadmBinary
)

var (
	defaultKubernetesURLTemplate = "https://acs-mirror.azureedge.net/kubernetes/v%s/binaries/kubernetes-node-linux-%s.tar.gz"
	kubernetesTarPath            = "kubernetes/node/bin/"
)

var kubeBinariesPaths = []string{
	kubeletPath,
	kubectlPath,
	kubeadmPath,
}

type downloadKubeBinariesAction struct{}

func newDownloadKubeBinariesAction() (actions.Server, error) {
	return &downloadKubeBinariesAction{}, nil
}

var _ actions.Server = (*downloadKubeBinariesAction)(nil)

func (d *downloadKubeBinariesAction) ApplyAction(
	ctx context.Context,
	req *actions.ApplyActionRequest,
) (*actions.ApplyActionResponse, error) {
	config, err := utilpb.AnyTo[*v20260301.DownloadKubeBinaries](req.GetItem())
	if err != nil {
		return nil, err
	}

	spec := config.GetSpec()

	if !d.canSkip(spec) {
		if err := d.download(ctx, spec); err != nil {
			return nil, err
		}
	}

	// TODO: capture status
	item, err := anypb.New(config)
	if err != nil {
		return nil, err
	}

	return actions.ApplyActionResponse_builder{Item: item}.Build(), nil
}

// canSkip checks if all Kube binaries are already installed with the correct version.
func (d *downloadKubeBinariesAction) canSkip(spec *v20260301.DownloadKubeBinariesSpec) bool {
	for _, binaryPath := range kubeBinariesPaths {
		if !utils.FileExists(binaryPath) {
			return false
		}
	}

	return d.isKubeletVersionCorrect(spec.GetKubernetesVersion())
}

// isKubeletVersionCorrect checks if the installed kubelet version matches the expected version.
func (d *downloadKubeBinariesAction) isKubeletVersionCorrect(expectedVersion string) bool {
	output, err := utils.RunCommandWithOutput(kubeletPath, "--version")
	if err != nil {
		return false
	}

	return strings.Contains(output, expectedVersion)
}

func (d *downloadKubeBinariesAction) download(
	ctx context.Context,
	spec *v20260301.DownloadKubeBinariesSpec,
) error {
	// Clean up any corrupted installations before proceeding
	d.cleanupExistingInstallation()

	url, err := d.constructDownloadURL(spec.GetKubernetesVersion())
	if err != nil {
		return status.Errorf(codes.Internal, "construct download URL: %s", err)
	}

	for tarFile, err := range utilio.DecompressTarGzFromRemote(ctx, url) {
		if err != nil {
			return status.Errorf(codes.Internal, "decompress tar: %s", err)
		}

		if !strings.HasPrefix(tarFile.Name, kubernetesTarPath) {
			continue
		}

		fileName := strings.TrimPrefix(tarFile.Name, kubernetesTarPath)
		targetFilePath := filepath.Join(binDir, fileName)

		if err := utilio.InstallFile(targetFilePath, tarFile.Body, 0755); err != nil {
			return status.Errorf(codes.Internal, "install file %q: %s", targetFilePath, err)
		}
	}

	return nil
}

// cleanupExistingInstallation removes any existing Kubernetes binaries that may be corrupted.
func (d *downloadKubeBinariesAction) cleanupExistingInstallation() {
	for _, binaryPath := range kubeBinariesPaths {
		if utils.FileExists(binaryPath) {
			_ = utils.RunCleanupCommand(binaryPath) //nolint:errcheck
		}
	}
}

// constructDownloadURL constructs the download URL for the specified Kubernetes version.
func (d *downloadKubeBinariesAction) constructDownloadURL(kubernetesVersion string) (string, error) {
	arch, err := utils.GetArc()
	if err != nil {
		return "", fmt.Errorf("get architecture: %w", err)
	}

	return fmt.Sprintf(defaultKubernetesURLTemplate, kubernetesVersion, arch), nil
}
