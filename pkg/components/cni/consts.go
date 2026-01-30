package cni

const (
	// CNI directories
	DefaultCNIBinDir  = "/opt/cni/bin"
	DefaultCNIConfDir = "/etc/cni/net.d"
	DefaultCNILibDir  = "/var/lib/cni"

	// CNI configuration files
	// Using 99-bridge.conf (high number) ensures CNI solutions like Cilium
	// can override this temporary bridge with lower-numbered configs (e.g., 05-cilium.conf)
	bridgeConfigFile = "99-bridge.conf"

	// Required CNI plugins
	bridgePlugin    = "bridge"
	hostLocalPlugin = "host-local"
	loopbackPlugin  = "loopback"
	portmapPlugin   = "portmap"
	bandwidthPlugin = "bandwidth"
	tuningPlugin    = "tuning"

	// CNI version
	defaultCNIVersion = "1.5.1"

	// CNI specification version for configuration files
	defaultCNISpecVersion = "0.3.1"
)

var cniDirs = []string{
	DefaultCNIBinDir,
	DefaultCNIConfDir,
	DefaultCNILibDir,
}

var requiredCNIPlugins = []string{
	bridgePlugin,
	hostLocalPlugin,
	loopbackPlugin,
}

var (
	cniFileName    = "cni-plugins-linux-%s-v%s.tgz"
	cniDownLoadURL = "https://github.com/containernetworking/plugins/releases/download/v%s/" + cniFileName
)
