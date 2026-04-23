package v20260301

import (
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/components/linux"
)

func TestRenderCheckRouteOverlapScript(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		cidrs        []string
		mode         linux.CheckRouteOverlapSpec_Mode
		wantContains []string
		wantErr      bool
	}{
		{
			name:  "empty CIDRs is a no-op",
			cidrs: nil,
			mode:  linux.CheckRouteOverlapSpec_WARN,
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
			mode:  linux.CheckRouteOverlapSpec_WARN,
			wantContains: []string{
				"mode=WARN",
				"ip -4 route get 172.16.0.1",
				`msg="OVERLAP: expected CIDR 172.16.0.0/16 (probe 172.16.0.1) routes via $ACTUAL, expected $DEFAULT_DEV"`,
				"if [ \"$bad\" -eq 1 ]; then",
				"  exit 0",
			},
		},
		{
			name:  "STRICT mode exits 1 on overlap",
			cidrs: []string{"172.16.0.0/16", "10.0.0.0/8"},
			mode:  linux.CheckRouteOverlapSpec_STRICT,
			wantContains: []string{
				"mode=STRICT",
				"ip -4 route get 172.16.0.1",
				"ip -4 route get 10.0.0.1",
				"if [ \"$bad\" -eq 1 ]; then",
				"  exit 1",
			},
		},
		{
			name:  "MODE_UNSPECIFIED defaults are caller's job; renderer treats it as WARN by exit code",
			cidrs: []string{"172.16.0.0/24"},
			mode:  linux.CheckRouteOverlapSpec_WARN,
			wantContains: []string{
				"mode=WARN",
				"  exit 0",
			},
		},
		{
			name:    "rejects invalid CIDR",
			cidrs:   []string{"not-a-cidr"},
			mode:    linux.CheckRouteOverlapSpec_WARN,
			wantErr: true,
		},
		{
			name:    "rejects IPv6 CIDR",
			cidrs:   []string{"2001:db8::/32"},
			mode:    linux.CheckRouteOverlapSpec_WARN,
			wantErr: true,
		},
		{
			name:  "single host /32 is probed at the address itself",
			cidrs: []string{"169.254.169.254/32"},
			mode:  linux.CheckRouteOverlapSpec_STRICT,
			wantContains: []string{
				"ip -4 route get 169.254.169.254",
			},
		},
		{
			name:  "always emits no-default-route guard",
			cidrs: []string{"172.16.0.0/24"},
			mode:  linux.CheckRouteOverlapSpec_STRICT,
			wantContains: []string{
				"no IPv4 default route; cannot determine outbound interface",
				`echo "no-default-route" > /run/aks-flex-node/route-overlap.detected`,
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
	got, err := renderCheckRouteOverlapScript([]string{"172.16.0.0/24"}, linux.CheckRouteOverlapSpec_STRICT)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The guard for "no default route" should use exit 1 in STRICT.
	guard := "echo \"no-default-route\" > /run/aks-flex-node/route-overlap.detected\n  exit 1\n"
	if !strings.Contains(got, guard) {
		t.Errorf("STRICT mode missing exit-1 in no-default-route guard\nscript:\n%s", got)
	}
}
