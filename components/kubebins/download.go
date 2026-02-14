package kubebins

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"
	utilexec "k8s.io/utils/exec"

	v20260301 "go.goms.io/aks/AKSFlexNode/components/kubebins/v20260301"
	"go.goms.io/aks/AKSFlexNode/components/services/actions"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
	"go.goms.io/aks/AKSFlexNode/pkg/utils/utilio"
	"go.goms.io/aks/AKSFlexNode/pkg/utils/utilpb"
)

const (
	defaultKubernetesURLTemplate = "https://acs-mirror.azureedge.net/kubernetes/v%s/binaries/kubernetes-node-linux-%s.tar.gz"
	kubernetesTarPath            = "kubernetes/node/bin/"

	binDir         = "/usr/local/bin"
	binPathKubelet = binDir + "/kubelet"
)

var binariesRequired = []string{
	filepath.Join(binDir, "kubeadm"),
	filepath.Join(binDir, "kubelet"),
	filepath.Join(binDir, "kubectl"),
	filepath.Join(binDir, "kube-proxy"),
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

	needDownload := !hasRequiredBinaries() || !kubeletVersionMatch(ctx, spec.GetKubernetesVersion())
	if needDownload {
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

func (d *downloadKubeBinariesAction) download(
	ctx context.Context,
	spec *v20260301.DownloadKubeBinariesSpec,
) error {
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

		binaryName := strings.TrimPrefix(tarFile.Name, kubernetesTarPath)

		targetFilePath := filepath.Join(binDir, binaryName)

		if err := utilio.InstallFile(targetFilePath, tarFile.Body, 0755); err != nil {
			return status.Errorf(codes.Internal, "install file %q: %s", targetFilePath, err)
		}
	}

	return nil
}

// constructDownloadURL constructs the download URL for the specified Kubernetes version.
func (d *downloadKubeBinariesAction) constructDownloadURL(kubernetesVersion string) (string, error) {
	arch, err := utils.GetArc()
	if err != nil {
		return "", fmt.Errorf("get architecture: %w", err)
	}

	return fmt.Sprintf(defaultKubernetesURLTemplate, kubernetesVersion, arch), nil
}

func hasRequiredBinaries() bool {
	for _, binaryPath := range binariesRequired {
		s, err := os.Stat(binaryPath)
		if err != nil {
			return false
		}
		// check if it's executable file
		if s.IsDir() || s.Mode()&0111 == 0 {
			return false
		}
	}
	return true
}

func kubeletVersionMatch(ctx context.Context, version string) bool {
	output, err := utilexec.New().
		CommandContext(ctx, binPathKubelet, "version", "--client", "-o", "json").
		Output()
	if err != nil {
		return false
	}
	var s struct {
		ClientVersion struct {
			GitVersion string `json:"gitVersion"`
		} `json:"clientVersion"`
	}
	if err := json.Unmarshal(output, &s); err != nil {
		return false
	}
	return s.ClientVersion.GitVersion == version
}
