package arc

const (
	arcAgentScriptURL     = "https://gbl.his.arc.azure.com/azcmagent-linux"
	arcAgentTmpScriptPath = "/tmp/install_linux_azcmagent_%d.sh"
	arcBinaryPath         = "/usr/local/bin/azcmagent"
)

var (
	// Map role names to role definition IDs
	roleDefinitionIDs = map[string]string{
		"Reader":              "acdd72a7-3385-48ef-bd42-f606fba81ae7",
		"Network Contributor": "4d97b98b-1d4f-4787-a291-c67834d212e7",
		"Contributor":         "b24988ac-6180-42a0-ab88-20f7382dd24c",
		"Azure Kubernetes Service RBAC Cluster Admin": "b1ff04bb-8a4e-4dc4-8eb5-8693973ce19b",
		"Azure Kubernetes Service Cluster Admin Role": "0ab0b1a8-8aac-4efd-b8c2-3ee1fb270be8",
	}

	arcPaths = []string{
		"/opt/azcmagent/bin/azcmagent",
		"/usr/local/bin/azcmagent",
		"/usr/bin/azcmagent",
	}

	arcServices = []string{"himdsd", "gcarcservice", "extd"}
)
