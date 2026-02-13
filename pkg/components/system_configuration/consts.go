package system_configuration

const (
	// Configuration file paths
	sysctlConfigPath = "/etc/sysctl.d/999-sysctl-aks.conf"
	resolvConfPath   = "/etc/resolv.conf"
	resolvConfSource = "/run/systemd/resolve/resolv.conf"
)
