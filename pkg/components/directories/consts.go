package directories

import "os"

// Directory permission mappings for security-sensitive directories
type DirectoryPermissions struct {
	Mode        os.FileMode
	Description string
}

// Directory permission constants
var DirectoryPermissionMap = map[string]DirectoryPermissions{
	"certs":   {0755, "certificates directory"},
	"kubelet": {0750, "kubelet data directory"},
	"default": {0755, "standard directory"},
}

// Standard directories that need to be created/cleaned up
var StandardDirectories = []string{
	"/etc/kubernetes",
	"/var/lib/kubelet",
	"/var/lib/containerd",
	"/etc/containerd",
	"/opt/cni/bin",
	"/etc/cni/net.d",
	"/var/log/pods",
	"/var/log/containers",
}
