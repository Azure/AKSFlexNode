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

func TestLocalDNSProfileCorefileOptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		override   LocalDNSOverride
		want       []string
		wantAbsent []string
	}{
		{
			name:       "VnetDNS PreferUDP sequential immediate",
			override:   LocalDNSOverride{QueryLogging: "Log", Protocol: "PreferUDP", ForwardDestination: "VnetDNS", ForwardPolicy: "Sequential", MaxConcurrent: 1000, CacheDurationInSeconds: 30, ServeStaleDurationInSeconds: 60, ServeStale: "Immediate"},
			want:       []string{"log", "forward . {{ .NodeUpstreamIPsJoined }}", "policy sequential", "max_concurrent 1000", "cache 30", "serve_stale 60s immediate"},
			wantAbsent: []string{"force_tcp"},
		},
		{
			name:     "ClusterCoreDNS ForceTCP round robin immediate",
			override: LocalDNSOverride{QueryLogging: "Error", Protocol: "ForceTCP", ForwardDestination: "ClusterCoreDNS", ForwardPolicy: "RoundRobin", ServeStale: "Immediate"},
			want:     []string{"errors", "forward . {{ .ClusterDNSServiceIP }}", "force_tcp", "policy round_robin", "serve_stale 3600s immediate"},
		},
		{
			name:       "random without stale",
			override:   LocalDNSOverride{ForwardDestination: "ClusterCoreDNS", ForwardPolicy: "Random", ServeStale: "Disable"},
			want:       []string{"policy random"},
			wantAbsent: []string{"serve_stale"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			vnetOverride := test.override
			vnetOverride.ForwardDestination = "VnetDNS"
			kubeOverride := test.override
			kubeOverride.ForwardDestination = "ClusterCoreDNS"
			profile := &LocalDNSProfile{
				Mode:             LocalDNSModeRequired,
				VnetDNSOverrides: map[string]LocalDNSOverride{".": vnetOverride},
				KubeDNSOverrides: map[string]LocalDNSOverride{".": kubeOverride},
			}
			got, err := profile.CorefileTemplate()
			if err != nil {
				t.Fatalf("CorefileTemplate() error = %v", err)
			}
			for _, want := range test.want {
				if !strings.Contains(got, want) {
					t.Errorf("CorefileTemplate() missing %q:\n%s", want, got)
				}
			}
			for _, unwanted := range test.wantAbsent {
				if strings.Contains(got, unwanted) {
					t.Errorf("CorefileTemplate() unexpectedly contains %q:\n%s", unwanted, got)
				}
			}
		})
	}
}

func TestLocalDNSProfileCorefileZoneOrder(t *testing.T) {
	t.Parallel()

	profile := &LocalDNSProfile{Mode: LocalDNSModeRequired, VnetDNSOverrides: map[string]LocalDNSOverride{
		"z.example": {},
		".":         {},
		"a.example": {},
	}}
	got, err := profile.CorefileTemplate()
	if err != nil {
		t.Fatal(err)
	}
	root := strings.Index(got, ".:53 {")
	a := strings.Index(got, "a.example:53 {")
	z := strings.Index(got, "z.example:53 {")
	if root < 0 || a < root || z < a {
		t.Fatalf("zones are not deterministic: root=%d a=%d z=%d\n%s", root, a, z, got)
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
		{name: "invalid logging", profile: &LocalDNSProfile{Mode: LocalDNSModeRequired, VnetDNSOverrides: map[string]LocalDNSOverride{".": {QueryLogging: "Debug"}}}, wantErr: "queryLogging"},
		{name: "invalid destination", profile: &LocalDNSProfile{Mode: LocalDNSModeRequired, VnetDNSOverrides: map[string]LocalDNSOverride{".": {ForwardDestination: "External"}}}, wantErr: "forwardDestination"},
		{name: "invalid policy", profile: &LocalDNSProfile{Mode: LocalDNSModeRequired, VnetDNSOverrides: map[string]LocalDNSOverride{".": {ForwardPolicy: "First"}}}, wantErr: "forwardPolicy"},
		{name: "invalid stale", profile: &LocalDNSProfile{Mode: LocalDNSModeRequired, VnetDNSOverrides: map[string]LocalDNSOverride{".": {ServeStale: "Always"}}}, wantErr: "serveStale"},
		{name: "negative cache", profile: &LocalDNSProfile{Mode: LocalDNSModeRequired, VnetDNSOverrides: map[string]LocalDNSOverride{".": {CacheDurationInSeconds: -1}}}, wantErr: "cacheDurationInSeconds"},
		{name: "VnetDNS root to cluster DNS", profile: &LocalDNSProfile{Mode: LocalDNSModeRequired, VnetDNSOverrides: map[string]LocalDNSOverride{".": {ForwardDestination: "ClusterCoreDNS"}}}, wantErr: "must not be ClusterCoreDNS"},
		{name: "cluster zone to VnetDNS", profile: &LocalDNSProfile{Mode: LocalDNSModeRequired, KubeDNSOverrides: map[string]LocalDNSOverride{"cluster.local": {ForwardDestination: "VnetDNS"}}}, wantErr: "must not be VnetDNS"},
		{name: "ForceTCP with stale verification", profile: &LocalDNSProfile{Mode: LocalDNSModeRequired, KubeDNSOverrides: map[string]LocalDNSOverride{".": {Protocol: "ForceTCP", ServeStale: "Verify"}}}, wantErr: "requires protocol PreferUDP"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := test.profile.Validate()
			if test.wantErr == "" && err != nil {
				t.Fatalf("validate() error = %v", err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("validate() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}
