package config

import "testing"

func boolPtr(v bool) *bool { return &v }

func TestIsDriftDetectionAndRemediationEnabled(t *testing.T) {
	t.Parallel()

	var nilCfg *Config
	if nilCfg.IsDriftDetectionAndRemediationEnabled() {
		t.Fatalf("nil config should return false")
	}

	// Backward compatible default: nil flag => enabled.
	cfg := &Config{}
	if !cfg.IsDriftDetectionAndRemediationEnabled() {
		t.Fatalf("nil EnableDriftDetectionAndRemediation should default to true")
	}

	cfg.Agent.EnableDriftDetectionAndRemediation = boolPtr(true)
	if !cfg.IsDriftDetectionAndRemediationEnabled() {
		t.Fatalf("flag true should return true")
	}

	cfg.Agent.EnableDriftDetectionAndRemediation = boolPtr(false)
	if cfg.IsDriftDetectionAndRemediationEnabled() {
		t.Fatalf("flag false should return false")
	}
}
