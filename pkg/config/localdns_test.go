package config

import (
	"strings"
	"testing"
)

func TestLocalDNSProfileCorefileTemplate(t *testing.T) {
	t.Parallel()

	profile := &LocalDNSProfile{
		Mode: LocalDNSModeRequired,
		VnetDNSOverrides: map[string]LocalDNSOverride{
			".": {ForwardDestination: "VnetDNS", Protocol: "PreferUDP"},
		},
		KubeDNSOverrides: map[string]LocalDNSOverride{
			".": {ForwardDestination: "ClusterCoreDNS", Protocol: "ForceTCP", ForwardPolicy: "RoundRobin"},
		},
	}
	got, err := profile.CorefileTemplate()
	if err != nil {
		t.Fatalf("CorefileTemplate() error = %v", err)
	}
	for _, want := range []string{
		"bind {{ .NodeListenerIP }}",
		"forward . {{ .NodeUpstreamIPsJoined }}",
		"bind {{ .ClusterListenerIP }}",
		"forward . {{ .ClusterDNSServiceIP }}",
		"force_tcp",
		"policy round_robin",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("CorefileTemplate() missing %q:\n%s", want, got)
		}
	}
}

func TestLocalDNSProfileValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		profile *LocalDNSProfile
		wantErr string
	}{
		{name: "required", profile: &LocalDNSProfile{Mode: LocalDNSModeRequired}},
		{name: "preferred", profile: &LocalDNSProfile{Mode: LocalDNSModePreferred}},
		{name: "disabled", profile: &LocalDNSProfile{Mode: LocalDNSModeDisabled}},
		{name: "invalid mode", profile: &LocalDNSProfile{Mode: "On"}, wantErr: "mode"},
		{name: "invalid protocol", profile: &LocalDNSProfile{Mode: LocalDNSModeRequired, VnetDNSOverrides: map[string]LocalDNSOverride{".": {Protocol: "TLS"}}}, wantErr: "protocol"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := test.profile.validate()
			if test.wantErr == "" && err != nil {
				t.Fatalf("validate() error = %v", err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("validate() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}
