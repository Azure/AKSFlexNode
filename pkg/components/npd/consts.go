package npd

// NPD binary paths to check and manage
const (
	npdBinaryPath  = "/usr/bin/node-problem-detector"
	npdConfigPath  = "/etc/node-problem-detector/kernel-monitor.json"
	npdServicePath = "/etc/systemd/system/node-problem-detector.service"
	tempDir        = "/tmp/npd"
)

var (
	npdFileName    = "npd-%s.tar.gz"
	npdDownloadURL = "https://github.com/kubernetes/node-problem-detector/releases/download/%s/node-problem-detector-%s-linux_%s.tar.gz"
)
