package v20260301

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"
	utilexec "k8s.io/utils/exec"

	"go.goms.io/aks/AKSFlexNode/components/kubebins"
	"go.goms.io/aks/AKSFlexNode/components/services/actions"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
	"go.goms.io/aks/AKSFlexNode/pkg/utils/utilhost"
	"go.goms.io/aks/AKSFlexNode/pkg/utils/utilio"
	"go.goms.io/aks/AKSFlexNode/pkg/utils/utilpb"
)

const (
	defaultKubernetesURLTemplate = "https://acs-mirror.azureedge.net/kubernetes/v%s/binaries/kubernetes-node-linux-%s.tar.gz"
	kubernetesTarPath            = "kubernetes/node/bin/"
)

var (
	binPathKubelet = filepath.Join(config.DefaultBinaryPath, "kubelet")

	binariesRequired = []string{
		filepath.Join(config.DefaultBinaryPath, "kubeadm"),
		filepath.Join(config.DefaultBinaryPath, "kubelet"),
		filepath.Join(config.DefaultBinaryPath, "kubectl"),
		filepath.Join(config.DefaultBinaryPath, "kube-proxy"),
	}
)

type downloadKubeBinariesAction struct{}

func newDownloadKubeBinariesAction() (actions.Server, error) {
	return &downloadKubeBinariesAction{}, nil
}

var _ actions.Server = (*downloadKubeBinariesAction)(nil)

func (d *downloadKubeBinariesAction) ApplyAction(
	ctx context.Context,
	req *actions.ApplyActionRequest,
) (*actions.ApplyActionResponse, error) {
	settings, err := utilpb.AnyTo[*kubebins.DownloadKubeBinaries](req.GetItem())
	if err != nil {
		return nil, err
	}

	spec := settings.GetSpec()

	downloadURL := d.constructDownloadURL(spec.GetKubernetesVersion())

	st := kubebins.DownloadKubeBinariesStatus_builder{
		DownloadUrl: utils.Ptr(downloadURL),
		KubeletPath: utils.Ptr(binPathKubelet),
		KubeadmPath: utils.Ptr(filepath.Join(config.DefaultBinaryPath, "kubeadm")),
		KubectlPath: utils.Ptr(filepath.Join(config.DefaultBinaryPath, "kubectl")),
	}

	needDownload := !hasRequiredBinaries() || !kubeletVersionMatch(ctx, spec.GetKubernetesVersion())
	if needDownload {
		if err := d.download(ctx, downloadURL); err != nil {
			return nil, err
		}
	}

	settings.SetStatus(st.Build())

	item, err := anypb.New(settings)
	if err != nil {
		return nil, err
	}

	return actions.ApplyActionResponse_builder{Item: item}.Build(), nil
}

func (d *downloadKubeBinariesAction) download(ctx context.Context, downloadURL string) error {
	for tarFile, err := range utilio.DecompressTarGzFromRemote(ctx, downloadURL) {
		if err != nil {
			return status.Errorf(codes.Internal, "decompress tar: %s", err)
		}

		if !strings.HasPrefix(tarFile.Name, kubernetesTarPath) {
			continue
		}

		binaryName := strings.TrimPrefix(tarFile.Name, kubernetesTarPath)

		targetFilePath := filepath.Join(config.DefaultBinaryPath, binaryName)

		if err := utilio.InstallFile(targetFilePath, tarFile.Body, 0755); err != nil {
			return status.Errorf(codes.Internal, "install file %q: %s", targetFilePath, err)
		}
	}

	return nil
}

// constructDownloadURL constructs the download URL for the specified Kubernetes version.
func (d *downloadKubeBinariesAction) constructDownloadURL(kubernetesVersion string) string {
	arch := utilhost.GetArch()
	return fmt.Sprintf(defaultKubernetesURLTemplate, kubernetesVersion, arch)
}

func hasRequiredBinaries() bool {
	for _, binaryPath := range binariesRequired {
		if !utilio.IsExecutable(binaryPath) {
			return false
		}
	}
	return true
}

func kubeletVersionMatch(ctx context.Context, version string) bool {
	output, err := utilexec.New().
		CommandContext(ctx, binPathKubelet, "--version").
		Output()
	if err != nil {
		return false
	}
	// output example: "Kubernetes v1.27.3"
	parts := strings.Fields(string(output))
	if len(parts) != 2 {
		return false
	}
	kubeletVersion := strings.TrimPrefix(parts[1], "v")
	return kubeletVersion == version
}
