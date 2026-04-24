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
			mode:  linux.CheckRouteOverlapSpec_STRICT,
			wantContains: []string{
				"mode=STRICT",
				"172.16.0.0/16|172.16.0.1",
				"10.0.0.0/8|10.0.0.1",
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
				"169.254.169.254/32|169.254.169.254",
			},
		},
		{
			name:  "always emits no-default-route guard",
			cidrs: []string{"172.16.0.0/24"},
			mode:  linux.CheckRouteOverlapSpec_STRICT,
			wantContains: []string{
				`awk '/^default / {for (i=1;i<=NF;i++) if ($i=="dev") {print $(i+1); exit}}'`,
				"no IPv4 default route; cannot determine outbound interface",
				`echo "no-default-route" > /run/aks-flex-node/route-overlap.detected`,
			},
		},
		{
			name:  "non-network-aligned prefix probes inside the CIDR",
			cidrs: []string{"10.0.0.255/24"},
			mode:  linux.CheckRouteOverlapSpec_WARN,
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

func TestRenderCheckRouteOverlapScriptAvoidsRepoSourcePathInGuidance(t *testing.T) {
	t.Parallel()
	got, err := renderCheckRouteOverlapScript([]string{"172.16.0.0/24"}, linux.CheckRouteOverlapSpec_WARN)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(got, "AKSFlexNode/components/linux/v20260301/configure_static_routes.go") {
		t.Fatalf("runtime guidance must not reference repository source paths:\n%s", got)
	}
}

func TestValidateCheckRouteOverlapSpec(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		spec    *linux.CheckRouteOverlapSpec
		wantErr bool
	}{
		{
			name:    "nil spec is rejected",
			spec:    nil,
			wantErr: true,
		},
		{
			name: "non-nil spec is accepted",
			spec: linux.CheckRouteOverlapSpec_builder{
				ExpectedCidrs: []string{"172.16.0.0/16"},
			}.Build(),
			wantErr: false,
		},
		{
			name: "unknown mode is rejected",
			spec: func() *linux.CheckRouteOverlapSpec {
				mode := linux.CheckRouteOverlapSpec_Mode(99)
				return linux.CheckRouteOverlapSpec_builder{
					Mode: &mode,
				}.Build()
			}(),
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateCheckRouteOverlapSpec(tc.spec)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
		})
	}
}
