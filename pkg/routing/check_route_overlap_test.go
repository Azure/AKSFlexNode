package routing

import (
	"strings"
	"testing"
)

func TestRenderCheckRouteOverlapScript(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		cidrs        []string
		mode         routeOverlapMode
		wantContains []string
		wantErr      bool
	}{
		{
			name:  "empty CIDRs is a no-op",
			cidrs: nil,
			mode:  routeOverlapWarn,
			wantContains: []string{
				"#!/bin/bash",
				"mode=WARN",
				"no expected CIDRs configured",
				"touch /run/aks-flex-node/route-overlap.ok",
				"exit 0",
			},
		},
		{
			name:  "WARN mode exits 0 on overlap",
			cidrs: []string{"172.16.0.0/16"},
			mode:  routeOverlapWarn,
			wantContains: []string{
				"mode=WARN",
				`ACTUAL=$(ip -4 route get "$PROBE"`,
				`msg="NO-ROUTE: expected CIDR $CIDR (probe $PROBE) has no IPv4 route; expected via $DEFAULT_DEV"`,
				`msg="OVERLAP: expected CIDR $CIDR (probe $PROBE) routes via $ACTUAL, expected $DEFAULT_DEV"`,
				"172.16.0.0/16|172.16.0.1",
				"if [ \"$bad\" -eq 1 ]; then",
				"For each affected CIDR, add a spec.staticRoutes entry with the",
				"  exit 0",
			},
		},
		{
			name:  "STRICT mode exits 1 on overlap",
			cidrs: []string{"172.16.0.0/16", "10.0.0.0/8"},
			mode:  routeOverlapStrict,
			wantContains: []string{
				"mode=STRICT",
				"172.16.0.0/16|172.16.0.1",
				"10.0.0.0/8|10.0.0.1",
				"if [ \"$bad\" -eq 1 ]; then",
				"  exit 1",
			},
		},
		{
			name:    "rejects invalid CIDR",
			cidrs:   []string{"not-a-cidr"},
			mode:    routeOverlapWarn,
			wantErr: true,
		},
		{
			name:    "rejects IPv6 CIDR",
			cidrs:   []string{"2001:db8::/32"},
			mode:    routeOverlapWarn,
			wantErr: true,
		},
		{
			name:  "single host /32 is probed at the address itself",
			cidrs: []string{"169.254.169.254/32"},
			mode:  routeOverlapStrict,
			wantContains: []string{
				"169.254.169.254/32|169.254.169.254",
			},
		},
		{
			name:  "always emits no-default-route guard",
			cidrs: []string{"172.16.0.0/24"},
			mode:  routeOverlapStrict,
			wantContains: []string{
				`awk '/^default / {for (i=1;i<=NF;i++) if ($i=="dev") {print $(i+1); exit}}'`,
				"no IPv4 default route; cannot determine outbound interface",
				`echo "no-default-route" > /run/aks-flex-node/route-overlap.detected`,
			},
		},
		{
			name:  "non-network-aligned prefix probes inside the CIDR",
			cidrs: []string{"10.0.0.255/24"},
			mode:  routeOverlapWarn,
			wantContains: []string{
				"10.0.0.255/24|10.0.0.1",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := renderCheckRouteOverlapScript(tc.cidrs, tc.mode)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got script:\n%s", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("script missing %q\nscript:\n%s", want, got)
				}
			}
		})
	}
}

func TestRenderCheckRouteOverlapScriptDefaultExitInGuard(t *testing.T) {
	// In STRICT mode the no-default-route guard must also exit 1.
	t.Parallel()
	got, err := renderCheckRouteOverlapScript([]string{"172.16.0.0/24"}, routeOverlapStrict)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	guard := "echo \"no-default-route\" > /run/aks-flex-node/route-overlap.detected\n  exit 1\n"
	if !strings.Contains(got, guard) {
		t.Errorf("STRICT mode missing exit-1 in no-default-route guard\nscript:\n%s", got)
	}
}

func TestParseRouteOverlapMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input   string
		want    routeOverlapMode
		wantErr bool
	}{
		{"", routeOverlapWarn, false},
		{"WARN", routeOverlapWarn, false},
		{"warn", routeOverlapWarn, false},
		{"STRICT", routeOverlapStrict, false},
		{"strict", routeOverlapStrict, false},
		{"UNKNOWN", 0, true},
		{"invalid", 0, true},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			got, err := parseRouteOverlapMode(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got nil", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.input, err)
			}
			if got != tc.want {
				t.Errorf("parseRouteOverlapMode(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}
