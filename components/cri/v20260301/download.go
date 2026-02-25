package v20260301

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/blang/semver/v4"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"
	utilexec "k8s.io/utils/exec"

	"go.goms.io/aks/AKSFlexNode/components/api"
	"go.goms.io/aks/AKSFlexNode/components/cri"
	"go.goms.io/aks/AKSFlexNode/components/services/actions"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/utils/utilhost"
	"go.goms.io/aks/AKSFlexNode/pkg/utils/utilio"
	"go.goms.io/aks/AKSFlexNode/pkg/utils/utilpb"
)

const (
	// containerd download URL template: version, version, arch
	defaultContainerdURLTemplate = "https://github.com/containerd/containerd/releases/download/v%s/containerd-%s-linux-%s.tar.gz"
	containerdTarPrefix          = "bin/"

	// runc download URL template: version, arch
	defaultRuncURLTemplate = "https://github.com/opencontainers/runc/releases/download/v%s/runc.%s"
)

var (
	containerdBinPath = filepath.Join(config.DefaultBinaryPath, "containerd")
	runcBinPath       = filepath.Join(config.DefaultBinaryPath, "runc")

	// containerdBinaries lists all binaries included in containerd releases.
	containerdBinaries = []string{
		"ctr",
		"containerd",
		"containerd-shim-runc-v2",
		"containerd-stress",
	}
)

type downloadCRIBinariesAction struct{}

func newDownloadCRIBinariesAction() (actions.Server, error) {
	return &downloadCRIBinariesAction{}, nil
}

var _ actions.Server = (*downloadCRIBinariesAction)(nil)

func (d *downloadCRIBinariesAction) ApplyAction(
	ctx context.Context,
	req *actions.ApplyActionRequest,
) (*actions.ApplyActionResponse, error) {
	config, err := utilpb.AnyTo[*cri.DownloadCRIBinaries](req.GetItem())
	if err != nil {
		return nil, err
	}

	spec, err := api.DefaultAndValidate(config.GetSpec()) // TODO: can we move this logic to hub side?
	if err != nil {
		return nil, err
	}

	containerdVersion, err := semver.Parse(spec.GetContainerdVersion())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid containerd version %q: %s", spec.GetContainerdVersion(), err)
	}
	if containerdVersion.Major < 2 {
		return nil, status.Errorf(codes.InvalidArgument, "containerd version %q is not supported, minimum required version is 2.0.0", spec.GetContainerdVersion())
	}

	containerdURL := constructContainerdDownloadURL(spec.GetContainerdVersion())
	runcURL := constructRuncDownloadURL(spec.GetRuncVersion())

	st := cri.DownloadCRIBinariesStatus_builder{
		ContainerdDownloadUrl: to.Ptr(containerdURL),
		ContainerdPath:        to.Ptr(containerdBinPath),
		RuncDownloadUrl:       to.Ptr(runcURL),
		RuncPath:              to.Ptr(runcBinPath),
	}

	if !containerdVersionMatch(spec.GetContainerdVersion()) {
		if err := d.downloadContainerd(ctx, containerdURL); err != nil {
			return nil, err
		}
	}

	if !runcVersionMatch(spec.GetRuncVersion()) {
		if err := d.downloadRunc(ctx, runcURL); err != nil {
			return nil, err
		}
	}

	config.SetStatus(st.Build())

	item, err := anypb.New(config)
	if err != nil {
		return nil, err
	}

	return actions.ApplyActionResponse_builder{Item: item}.Build(), nil
}

// downloadContainerd downloads and extracts containerd binaries from a tar.gz archive.
func (d *downloadCRIBinariesAction) downloadContainerd(
	ctx context.Context,
	downloadURL string,
) error {
	for tarFile, err := range utilio.DecompressTarGzFromRemote(ctx, downloadURL) {
		if err != nil {
			return status.Errorf(codes.Internal, "decompress containerd tar: %s", err)
		}

		if !strings.HasPrefix(tarFile.Name, containerdTarPrefix) {
			continue
		}

		binaryName := strings.TrimPrefix(tarFile.Name, containerdTarPrefix)
		targetFilePath := filepath.Join(config.DefaultBinaryPath, binaryName)

		if err := utilio.InstallFile(targetFilePath, tarFile.Body, 0755); err != nil {
			return status.Errorf(codes.Internal, "install containerd file %q: %s", targetFilePath, err)
		}
	}

	return nil
}

// downloadRunc downloads the runc binary directly.
func (d *downloadCRIBinariesAction) downloadRunc(ctx context.Context, downloadURL string) error {
	if err := utilio.DownloadToLocalFile(ctx, downloadURL, runcBinPath, 0755); err != nil {
		return status.Errorf(codes.Internal, "download runc: %s", err)
	}

	return nil
}

// containerdVersionMatch checks if the installed containerd version matches the expected version.
func containerdVersionMatch(expectedVersion string) bool {
	for _, binary := range containerdBinaries {
		binaryPath := filepath.Join(config.DefaultBinaryPath, binary)
		if !utilio.IsExecutable(binaryPath) {
			return false
		}
	}

	output, err := utilexec.New().Command(containerdBinPath, "--version").Output()
	if err != nil {
		return false
	}

	return strings.Contains(string(output), expectedVersion) // FIXME: this is not a robust way
}

// runcVersionMatch checks if the installed runc version matches the expected version.
func runcVersionMatch(expectedVersion string) bool {
	if !utilio.IsExecutable(runcBinPath) {
		return false
	}

	output, err := utilexec.New().Command(runcBinPath, "--version").Output()
	if err != nil {
		return false
	}

	return strings.Contains(string(output), expectedVersion) // FIXME: this is not a robust way
}

// constructContainerdDownloadURL builds the download URL for the given containerd version.
func constructContainerdDownloadURL(version string) string {
	arch := utilhost.GetArch()
	return fmt.Sprintf(defaultContainerdURLTemplate, version, version, arch)
}

// constructRuncDownloadURL builds the download URL for the given runc version.
func constructRuncDownloadURL(version string) string {
	arch := utilhost.GetArch()
	return fmt.Sprintf(defaultRuncURLTemplate, version, arch)
}
