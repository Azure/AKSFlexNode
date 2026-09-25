// Package scripts embeds the host scripts that the aks-flex-node binary hands to hosts.
package scripts

import _ "embed"

// Bootstrap is bootstrap.sh, which `aks-flex-node ignition` writes to hosts that run it on first
// boot.
//
//go:embed bootstrap.sh
var Bootstrap string
