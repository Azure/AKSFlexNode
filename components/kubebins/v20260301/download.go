package v20260301

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"
	utilexec "k8s.io/utils/exec"

	"github.com/Azure/AKSFlexNode/components/kubebins"
	"github.com/Azure/AKSFlexNode/components/services/actions"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/logger"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilhost"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilpb"
)

const (
	// kubernetesBinaryURLTemplate is the download URL template for individual Kubernetes binaries
	// from the official Kubernetes release CDN (https://dl.k8s.io).
	// Parameters: version, arch, binary name.
	kubernetesBinaryURLTemplate = "https://dl.k8s.io/v%s/bin/linux/%s/%s"
)

// requiredBinaries lists the Kubernetes binaries that must be present for a valid installation.
var requiredBinaries = []string{
	"kubeadm",
	"kubelet",
	"kubectl",
	"kube-proxy",
}

var binPathKubelet = filepath.Join(config.DefaultBinaryPath, "kubelet")

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

	version := spec.GetKubernetesVersion()
	arch := utilhost.GetArch()

	st := kubebins.DownloadKubeBinariesStatus_builder{
		KubeletPath:          to.Ptr(binPathKubelet),
		KubeadmPath:          to.Ptr(filepath.Join(config.DefaultBinaryPath, "kubeadm")),
		KubectlPath:          to.Ptr(filepath.Join(config.DefaultBinaryPath, "kubectl")),
		KubeProxyPath:        to.Ptr(filepath.Join(config.DefaultBinaryPath, "kube-proxy")),
		KubeletDownloadUrl:   to.Ptr(fmt.Sprintf(kubernetesBinaryURLTemplate, version, arch, "kubelet")),
		KubeadmDownloadUrl:   to.Ptr(fmt.Sprintf(kubernetesBinaryURLTemplate, version, arch, "kubeadm")),
		KubectlDownloadUrl:   to.Ptr(fmt.Sprintf(kubernetesBinaryURLTemplate, version, arch, "kubectl")),
		KubeProxyDownloadUrl: to.Ptr(fmt.Sprintf(kubernetesBinaryURLTemplate, version, arch, "kube-proxy")),
	}

	needDownload := !hasRequiredBinaries() || !kubeletVersionMatch(ctx, version)
	if needDownload {
		if err := d.download(ctx, version); err != nil {
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

// download fetches all required Kubernetes binaries in parallel from dl.k8s.io.
func (d *downloadKubeBinariesAction) download(ctx context.Context, kubernetesVersion string) error {
	arch := utilhost.GetArch()
	log := logger.GetLoggerFromContext(ctx)

	eg, ctx := errgroup.WithContext(ctx)

	for _, binary := range requiredBinaries {
		binaryURL := fmt.Sprintf(kubernetesBinaryURLTemplate, kubernetesVersion, arch, binary)
		targetFilePath := filepath.Join(config.DefaultBinaryPath, binary)

		eg.Go(func() error {
			log.WithField("binary", binary).WithField("url", binaryURL).Info("downloading kubernetes binary")

			start := time.Now()

			if err := utilio.DownloadToLocalFile(ctx, binaryURL, targetFilePath, 0755); err != nil {
				return status.Errorf(codes.Internal, "download kubernetes binary %q: %s", binary, err)
			}

			log.WithField("binary", binary).WithField("duration", time.Since(start)).Info("downloaded kubernetes binary")

			return nil
		})
	}

	return eg.Wait()
}

// hasRequiredBinaries checks if all required Kubernetes binaries are installed and executable.
func hasRequiredBinaries() bool {
	for _, binary := range requiredBinaries {
		binaryPath := filepath.Join(config.DefaultBinaryPath, binary)
		if !utilio.IsExecutable(binaryPath) {
			return false
		}
	}
	return true
}

// kubeletVersionMatch checks if the installed kubelet version matches the expected version.
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
