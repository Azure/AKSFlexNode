package v20260301

import (
	"context"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"
	utilexec "k8s.io/utils/exec"

	"go.goms.io/aks/AKSFlexNode/components/api"
	"go.goms.io/aks/AKSFlexNode/components/npd"
	"go.goms.io/aks/AKSFlexNode/components/services/actions"
	"go.goms.io/aks/AKSFlexNode/pkg/utils/utilhost"
	"go.goms.io/aks/AKSFlexNode/pkg/utils/utilio"
	"go.goms.io/aks/AKSFlexNode/pkg/utils/utilpb"
)

const (
	defaultNPDURLTemplate = "https://github.com/kubernetes/node-problem-detector/releases/download/%s/node-problem-detector-%s-linux_%s.tar.gz"

	npdBinaryPath = "/usr/bin/node-problem-detector"
	npdConfigPath = "/etc/node-problem-detector/kernel-monitor.json"
)

type downloadNodeProblemDetectorAction struct{}

func newDownloadNodeProblemDetectorAction() (actions.Server, error) {
	return &downloadNodeProblemDetectorAction{}, nil
}

var _ actions.Server = (*downloadNodeProblemDetectorAction)(nil)

func (d *downloadNodeProblemDetectorAction) ApplyAction(
	ctx context.Context,
	req *actions.ApplyActionRequest,
) (*actions.ApplyActionResponse, error) {
	settings, err := utilpb.AnyTo[*npd.DownloadNodeProblemDetector](req.GetItem())
	if err != nil {
		return nil, err
	}

	spec, err := api.DefaultAndValidate(settings.GetSpec())
	if err != nil {
		return nil, err
	}

	downloadURL := constructNPDDownloadURL(spec.GetVersion())
	if !npdVersionMatch(spec.GetVersion()) {
		if err := d.download(ctx, downloadURL); err != nil {
			return nil, err
		}
	}

	st := npd.DownloadNodeProblemDetectorStatus_builder{
		DownloadUrl: to.Ptr(downloadURL),
		BinaryPath:  to.Ptr(npdBinaryPath),
		ConfigPath:  to.Ptr(npdConfigPath),
	}

	settings.SetStatus(st.Build())

	item, err := anypb.New(settings)
	if err != nil {
		return nil, err
	}

	return actions.ApplyActionResponse_builder{Item: item}.Build(), nil
}

func (d *downloadNodeProblemDetectorAction) download(ctx context.Context, downloadURL string) error {
	for tarFile, err := range utilio.DecompressTarGzFromRemote(ctx, downloadURL) {
		if err != nil {
			return status.Errorf(codes.Internal, "decompress npd tar: %s", err)
		}

		switch tarFile.Name {
		case "bin/node-problem-detector":
			if err := utilio.InstallFile(npdBinaryPath, tarFile.Body, 0755); err != nil {
				return status.Errorf(codes.Internal, "install npd binary: %s", err)
			}
		case "config/kernel-monitor.json":
			if err := utilio.InstallFile(npdConfigPath, tarFile.Body, 0644); err != nil {
				return status.Errorf(codes.Internal, "install npd config: %s", err)
			}
		default:
			continue
		}
	}

	return nil
}

// constructNPDDownloadURL builds the download URL for the given NPD version.
func constructNPDDownloadURL(version string) string {
	arch := utilhost.GetArch()
	return fmt.Sprintf(defaultNPDURLTemplate, version, version, arch)
}

// npdVersionMatch checks if the installed NPD version matches the expected version.
func npdVersionMatch(expectedVersion string) bool {
	if !utilio.IsExecutable(npdBinaryPath) {
		return false
	}

	output, err := utilexec.New().Command(npdBinaryPath, "--version").Output()
	if err != nil {
		return false
	}

	return strings.Contains(string(output), expectedVersion)
}
