package cluster_credentials

// Credential file paths
const (
	AdminConfPath             = "/etc/kubernetes/admin.conf"
	ControllerManagerConfPath = "/etc/kubernetes/controller-manager.conf"
	SchedulerConfPath         = "/etc/kubernetes/scheduler.conf"
	PKIDir                    = "/etc/kubernetes/pki/"
)

// Credential files to manage
var CredentialFiles = []string{
	AdminConfPath,
	ControllerManagerConfPath,
	SchedulerConfPath,
	PKIDir,
}
