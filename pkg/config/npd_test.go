package config

import (
	"encoding/json"
	"testing"
)

func TestNPDDefaults(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		input   string
		enabled bool
		version string
	}{
		"omitted":      {input: `{}`, version: "v1.35.1"},
		"disabled":     {input: `{"npd":{"enabled":false}}`, version: "v1.35.1"},
		"enabled":      {input: `{"npd":{"enabled":true}}`, enabled: true, version: "v1.35.1"},
		"version only": {input: `{"npd":{"version":"v1.2.3"}}`, version: "v1.2.3"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var cfg Config
			if err := json.Unmarshal([]byte(tc.input), &cfg); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				cfg.setDefaults()
				if cfg.Npd.Enabled != tc.enabled || cfg.Npd.Version != tc.version {
					t.Fatalf("NPD config = %+v, want enabled=%v version=%s", cfg.Npd, tc.enabled, tc.version)
				}
			}
		})
	}
}
