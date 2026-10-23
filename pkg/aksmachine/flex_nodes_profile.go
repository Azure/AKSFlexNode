package aksmachine

import (
	"encoding/json"
	"fmt"
)

// disconnectToleranceFromMachineJSON reads properties.flexNodesProfile.disconnectTolerance.enabled,
// which the current SDK model omits.
// Missing intent means false. Malformed intent returns an error so callers keep current state.
// Errors never include response content.
// TODO: when armcontainerservice.MachineProperties includes FlexNodesProfile, read it in
// machineFromARM and remove this file plus the raw-response decoding in both Machine clients.
func disconnectToleranceFromMachineJSON(data []byte) (bool, error) {
	var body struct {
		Properties *struct {
			FlexNodesProfile json.RawMessage `json:"flexNodesProfile"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return false, fmt.Errorf("decode machine response for disconnect tolerance")
	}
	if body.Properties == nil || body.Properties.FlexNodesProfile == nil {
		return false, nil
	}
	var profile map[string]json.RawMessage
	if string(body.Properties.FlexNodesProfile) == "null" || json.Unmarshal(body.Properties.FlexNodesProfile, &profile) != nil {
		return false, fmt.Errorf("machine flexNodesProfile: value is not an object")
	}
	dt, ok := profile["disconnectTolerance"]
	if !ok {
		return false, nil
	}
	enabled, err := decodeDisconnectTolerance(dt)
	if err != nil {
		return false, fmt.Errorf("machine flexNodesProfile.disconnectTolerance: %w", err)
	}
	return enabled, nil
}

// decodeDisconnectTolerance decodes a present {"enabled": <bool>} object.
// A missing "enabled" means false; any other shape is an error.
func decodeDisconnectTolerance(raw json.RawMessage) (bool, error) {
	var profile map[string]json.RawMessage
	if string(raw) == "null" || json.Unmarshal(raw, &profile) != nil {
		return false, fmt.Errorf("value is not an object")
	}
	value, ok := profile["enabled"]
	if !ok {
		return false, nil
	}
	var enabled bool
	if string(value) == "null" || json.Unmarshal(value, &enabled) != nil {
		return false, fmt.Errorf("enabled is not a boolean")
	}
	return enabled, nil
}
