package npd

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.goms.io/aks/AKSFlexNode/pkg/config"
)

func (x *DownloadNodeProblemDetectorSpec) Defaulting() {
	if !x.HasVersion() {
		x.SetVersion(config.DefaultNPDVersion)
	}
}

func (x *StartNodeProblemDetectorSpec) Validate() error {
	if x.GetApiServer() == "" {
		return status.Error(codes.InvalidArgument, "ApiServer is required")
	}
	if x.GetKubeConfigPath() == "" {
		return status.Error(codes.InvalidArgument, "KubeConfigPath is required")
	}

	return nil
}
