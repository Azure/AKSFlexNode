package config

import "github.com/Azure/AKSFlexNode/pkg/config/internal/localdns"

const (
	LocalDNSModeRequired  = localdns.LocalDNSModeRequired
	LocalDNSModePreferred = localdns.LocalDNSModePreferred
	LocalDNSModeDisabled  = localdns.LocalDNSModeDisabled
)

// LocalDNSProfile is the AKS node-pool LocalDNS configuration contract.
type LocalDNSProfile = localdns.LocalDNSProfile

// LocalDNSOverride configures one AKS LocalDNS server block.
type LocalDNSOverride = localdns.LocalDNSOverride
