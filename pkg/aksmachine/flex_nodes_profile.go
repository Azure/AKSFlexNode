package aksmachine

import (
	"fmt"

	"github.com/tidwall/gjson"
)

// disconnectToleranceFromMachineJSON reads properties.flexNodesProfile.disconnectTolerance.enabled,
// which the current SDK model omits.
// Missing intent means false. Malformed intent returns an error so callers keep current state.
// Errors never include response content.
// TODO: when armcontainerservice.MachineProperties includes FlexNodesProfile, read it in
// machineFromARM and remove this file, the gjson dependency and the raw-response decoding
// in both Machine clients.
func disconnectToleranceFromMachineJSON(data []byte) (bool, error) {
	if !gjson.ValidBytes(data) {
		return false, fmt.Errorf("decode machine response for disconnect tolerance")
	}
	machine := gjson.ParseBytes(data)
	if !machine.IsObject() {
		return false, fmt.Errorf("decode machine response for disconnect tolerance")
	}
	// Each level must be an object when present; gjson alone would read a
	// malformed parent as missing, which means false and would disable Hub.
	for _, path := range []string{"properties", "properties.flexNodesProfile", "properties.flexNodesProfile.disconnectTolerance"} {
		value := machine.Get(path)
		if !value.Exists() || (path == "properties" && value.Type == gjson.Null) {
			return false, nil
		}
		if !value.IsObject() {
			return false, fmt.Errorf("machine %s: value is not an object", path)
		}
	}
	enabled := machine.Get("properties.flexNodesProfile.disconnectTolerance.enabled")
	if !enabled.Exists() {
		return false, nil
	}
	if !enabled.IsBool() {
		return false, fmt.Errorf("machine disconnectTolerance: enabled is not a boolean")
	}
	return enabled.Bool(), nil
}
