package services

import "time"

const (
	// Service names
	ContainerdService = "containerd"
	KubeletService    = "kubelet"

	// Service startup timeout
	ServiceStartupTimeout = 30 * time.Second
)
