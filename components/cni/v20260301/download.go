package v20260301

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"
	utilexec "k8s.io/utils/exec"

	"github.com/Azure/AKSFlexNode/components/api"
	"github.com/Azure/AKSFlexNode/components/cni"
	"github.com/Azure/AKSFlexNode/components/services/actions"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilhost"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilpb"
)

const (
	// CNI plugins download URL template: version, version, arch
	defaultCNIPluginsURLTemplate = "https://github.com/containernetworking/plugins/releases/download/v%s/cni-plugins-linux-%s-v%s.tgz"
)

var (
	// requiredCNIPlugins lists the CNI plugins that must be present for a valid installation.
	requiredCNIPlugins = []string{
		"bridge",
		"host-local",
		"loopback",
	}
)

type downloadCNIBinariesAction struct{}

func newDownloadCNIBinariesAction() (actions.Server, error) {
	return &downloadCNIBinariesAction{}, nil
}

var _ actions.Server = (*downloadCNIBinariesAction)(nil)

func (d *downloadCNIBinariesAction) ApplyAction(
	ctx context.Context,
	req *actions.ApplyActionRequest,
) (*actions.ApplyActionResponse, error) {
	settings, err := utilpb.AnyTo[*cni.DownloadCNIBinaries](req.GetItem())
	if err != nil {
		return nil, err
	}

	spec, err := api.DefaultAndValidate(settings.GetSpec())
	if err != nil {
		return nil, err
	}

	downloadURL := d.constructDownloadURL(spec.GetCniPluginsVersion())

	st := cni.DownloadCNIBinariesStatus_builder{
		CniPluginsDownloadUrl: to.Ptr(downloadURL),
		CniPluginsPath:        to.Ptr(config.DefaultCNIBinDir),
	}

	needDownload := !hasRequiredPlugins() || !cniPluginsVersionMatch(ctx, spec.GetCniPluginsVersion())
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

// download downloads and extracts CNI plugin binaries from a tar.gz archive.
func (d *downloadCNIBinariesAction) download(ctx context.Context, downloadURL string) error {
	for tarFile, err := range utilio.DecompressTarGzFromRemote(ctx, downloadURL) {
		if err != nil {
			return status.Errorf(codes.Internal, "decompress CNI plugins tar: %s", err)
		}

		targetFilePath := filepath.Join(config.DefaultCNIBinDir, tarFile.Name)

		if err := utilio.InstallFile(targetFilePath, tarFile.Body, 0755); err != nil {
			return status.Errorf(codes.Internal, "install CNI plugin %q: %s", targetFilePath, err)
		}
	}

	return nil
}

// constructDownloadURL builds the download URL for the given CNI plugins version.
func (d *downloadCNIBinariesAction) constructDownloadURL(cniPluginsVersion string) string {
	arch := utilhost.GetArch()
	return fmt.Sprintf(defaultCNIPluginsURLTemplate, cniPluginsVersion, arch, cniPluginsVersion)
}

// hasRequiredPlugins checks if all required CNI plugins are installed and executable.
func hasRequiredPlugins() bool {
	for _, plugin := range requiredCNIPlugins {
		pluginPath := filepath.Join(config.DefaultCNIBinDir, plugin)
		if !utilio.IsExecutable(pluginPath) {
			return false
		}
	}
	return true
}

// cniPluginsVersionMatch checks if the installed CNI plugins version matches the expected version.
func cniPluginsVersionMatch(ctx context.Context, expectedVersion string) bool {
	// Use the loopback plugin as the version check reference, as it is always present.
	loopbackPath := filepath.Join(config.DefaultCNIBinDir, "loopback")
	if !utilio.IsExecutable(loopbackPath) {
		return false
	}

	output, err := utilexec.New().
		CommandContext(ctx, loopbackPath, "--version").
		CombinedOutput()
	if err != nil {
		// Some CNI plugin versions don't support --version; treat as not matching.
		return false
	}

	return strings.Contains(string(output), expectedVersion)
}
