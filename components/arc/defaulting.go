package arc

import (
	"os"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Defaulting sets default values for InstallArcSpec fields
func (x *InstallArcSpec) Defaulting() {
	if !x.HasMachineName() {
		if hostname, err := os.Hostname(); err == nil && hostname != "" {
			x.SetMachineName(hostname)
		}
	}

	if x.GetTags() == nil {
		x.SetTags(make(map[string]string))
	}
}

// Validate checks if all required fields are present and valid
func (x *InstallArcSpec) Validate() error {
	if !x.HasSubscriptionId() || strings.TrimSpace(x.GetSubscriptionId()) == "" {
		return status.Error(codes.InvalidArgument, "SubscriptionId is required")
	}

	if !x.HasTenantId() || strings.TrimSpace(x.GetTenantId()) == "" {
		return status.Error(codes.InvalidArgument, "TenantId is required")
	}

	if !x.HasResourceGroup() || strings.TrimSpace(x.GetResourceGroup()) == "" {
		return status.Error(codes.InvalidArgument, "ResourceGroup is required")
	}

	if !x.HasLocation() || strings.TrimSpace(x.GetLocation()) == "" {
		return status.Error(codes.InvalidArgument, "Location is required")
	}

	if !x.HasMachineName() || strings.TrimSpace(x.GetMachineName()) == "" {
		return status.Error(codes.InvalidArgument, "MachineName is required")
	}

	if !x.HasAksClusterName() || strings.TrimSpace(x.GetAksClusterName()) == "" {
		return status.Error(codes.InvalidArgument, "AksClusterName is required")
	}

	return nil
}

// Defaulting sets default values for InstallArc fields
func (x *InstallArc) Defaulting() {
	if !x.HasSpec() {
		x.SetSpec(&InstallArcSpec{})
	}
	x.GetSpec().Defaulting()
}

// Validate checks if all required fields are present and valid
func (x *InstallArc) Validate() error {
	if !x.HasSpec() {
		return status.Error(codes.InvalidArgument, "Spec is required")
	}

	if err := x.GetSpec().Validate(); err != nil {
		return err
	}

	return nil
}
