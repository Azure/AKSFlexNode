package config

import (
	"encoding/json"
	"testing"
)

func TestBootstrapDisconnectToleranceEnabled(t *testing.T) {
	t.Parallel()
	// Shapes match RP listBootstrapData node output after bootstrap.sh merges it.
	tests := map[string]struct {
		json    string
		want    bool
		wantErr bool
	}{
		"true":            {json: `{"node":{"disconnectTolerance":{"enabled":true}}}`, want: true},
		"false":           {json: `{"node":{"disconnectTolerance":{"enabled":false}}}`},
		"enabled omitted": {json: `{"node":{"disconnectTolerance":{}}}`},
		"omitted":         {json: `{"node":{"maxPods":110}}`},
		"no node":         {json: `{}`},
		// A malformed value must not fail config loading (and the join).
		"wrong type":    {json: `{"node":{"disconnectTolerance":{"enabled":"true"}}}`},
		"not object":    {json: `{"node":{"disconnectTolerance":true}}`},
		"null":          {json: `{"node":{"disconnectTolerance":null}}`},
		"old flat bool": {json: `{"node":{"disconnectToleranceEnabled":true}}`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var cfg Config
			err := json.Unmarshal([]byte(tt.json), &cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Unmarshal error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if got := cfg.BootstrapDisconnectToleranceEnabled(); got != tt.want {
				t.Fatalf("BootstrapDisconnectToleranceEnabled() = %v, want %v", got, tt.want)
			}
			copied := cfg.DeepCopy()
			if copied == nil || copied.BootstrapDisconnectToleranceEnabled() != tt.want {
				t.Fatal("DeepCopy lost bootstrap intent")
			}
		})
	}
	if (*Config)(nil).BootstrapDisconnectToleranceEnabled() {
		t.Fatal("nil config reported enabled")
	}
}

func TestMachineClientConfigValidateAPIVersion(t *testing.T) {
	t.Parallel()
	for version, wantErr := range map[string]bool{
		"":                   false,
		"2026-10-02-preview": false,
		"2026-10-01":         false,
		"latest":             true,
		"2026-10-02-beta":    true,
	} {
		err := (MachineClientConfig{Mode: MachineClientModeARM, APIVersion: version}).validate()
		if (err != nil) != wantErr {
			t.Errorf("validate(apiVersion=%q) error = %v, wantErr %v", version, err, wantErr)
		}
	}
}
