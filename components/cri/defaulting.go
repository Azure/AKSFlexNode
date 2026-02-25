package cri

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.goms.io/aks/AKSFlexNode/pkg/config"
)

func (x *StartContainerdServiceSpec) Defaulting() {
	if !x.HasMetricsAddress() {
		x.SetMetricsAddress(config.DefaultContainerdMetricsAddress)
	}
	if !x.HasSandboxImage() {
		x.SetSandboxImage(config.DefaultSandboxImage)
	}

	if !x.HasCniConfig() {
		x.SetCniConfig(&CNIConfig{})
	}
	x.GetCniConfig().Defaulting()
}

func (x *StartContainerdServiceSpec) Validate() error {
	if !x.HasMetricsAddress() {
		return status.Error(codes.InvalidArgument, "MetricsAddress is required")
	}
	if !x.HasSandboxImage() {
		return status.Error(codes.InvalidArgument, "SandboxImage is required")
	}

	if err := x.GetCniConfig().Validate(); err != nil {
		return err
	}

	return nil
}

func (x *CNIConfig) Defaulting() {
	if !x.HasBinDir() {
		x.SetBinDir(config.DefaultCNIBinDir)
	}
	if !x.HasConfigDir() {
		x.SetConfigDir(config.DefaultCNIConfigDir)
	}
}

func (x *CNIConfig) Validate() error {
	if !x.HasBinDir() {
		return status.Error(codes.InvalidArgument, "CNI BinDir is required")
	}
	if !x.HasConfigDir() {
		return status.Error(codes.InvalidArgument, "CNI ConfigDir is required")
	}
	return nil
}
