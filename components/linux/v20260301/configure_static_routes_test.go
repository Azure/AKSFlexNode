package v20260301

import (
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"

	"github.com/Azure/AKSFlexNode/components/linux"
)

func TestRenderStaticRoutesScript(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		routes       []*linux.StaticRoute
		wantContains []string
		wantErr      bool
	}{
		{
			name:   "empty is no-op",
			routes: nil,
			wantContains: []string{
				"#!/bin/bash",
				"No routes configured",
				"exit 0",
			},
		},
		{
			name: "single route with explicit gateway",
			routes: []*linux.StaticRoute{
				linux.StaticRoute_builder{
					Destination: to.Ptr("172.16.1.0/24"),
					Gateway:     to.Ptr("172.18.1.1"),
					Dev:         to.Ptr("eth0"),
				}.Build(),
			},
			wantContains: []string{
				`GW="172.18.1.1"`,
				`ip route replace 172.16.1.0/24 via "$GW" dev "eth0"`,
			},
		},
		{
			name: "auto-resolve gateway when empty",
			routes: []*linux.StaticRoute{
				linux.StaticRoute_builder{
					Destination: to.Ptr("172.16.2.0/24"),
				}.Build(),
			},
			wantContains: []string{
				`GW=$(resolve_default_gw "eth0")`,
				`if [ -z "$GW" ]; then`,
				`ip route replace 172.16.2.0/24 via "$GW" dev "eth0"`,
			},
		},
		{
			name: "metric included when nonzero",
			routes: []*linux.StaticRoute{
				linux.StaticRoute_builder{
					Destination: to.Ptr("10.0.0.0/8"),
					Gateway:     to.Ptr("10.1.0.1"),
					Metric:      to.Ptr[uint32](100),
				}.Build(),
			},
			wantContains: []string{"metric 100"},
		},
		{
			name: "rejects invalid destination",
			routes: []*linux.StaticRoute{
				linux.StaticRoute_builder{Destination: to.Ptr("not-a-cidr")}.Build(),
			},
			wantErr: true,
		},
		{
			name: "rejects invalid gateway",
			routes: []*linux.StaticRoute{
				linux.StaticRoute_builder{
					Destination: to.Ptr("172.16.1.0/24"),
					Gateway:     to.Ptr("not-an-ip"),
				}.Build(),
			},
			wantErr: true,
		},
		{
			name: "rejects shell-meta in dev name",
			routes: []*linux.StaticRoute{
				linux.StaticRoute_builder{
					Destination: to.Ptr("172.16.1.0/24"),
					Dev:         to.Ptr("eth0; rm -rf /"),
				}.Build(),
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := renderStaticRoutesScript(tc.routes)
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

func TestRenderStaticRoutesScriptIsDeterministic(t *testing.T) {
	t.Parallel()
	routes := []*linux.StaticRoute{
		linux.StaticRoute_builder{Destination: to.Ptr("172.16.1.0/24"), Gateway: to.Ptr("172.18.1.1")}.Build(),
		linux.StaticRoute_builder{Destination: to.Ptr("172.16.2.0/24")}.Build(),
	}
	first, err := renderStaticRoutesScript(routes)
	if err != nil {
		t.Fatalf("first render: %v", err)
	}
	second, err := renderStaticRoutesScript(routes)
	if err != nil {
		t.Fatalf("second render: %v", err)
	}
	if first != second {
		t.Errorf("non-deterministic output.\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestIsSafeIfaceName(t *testing.T) {
	t.Parallel()
	tests := map[string]bool{
		"eth0":              true,
		"ib6":               true,
		"bond0.100":         true,
		"veth-foo_bar":      true,
		"":                  false,
		"toolong123456789a": false,
		"eth0; rm -rf /":    false,
		"$(whoami)":         false,
		"eth0 eth1":         false,
	}
	for in, want := range tests {
		if got := isSafeIfaceName(in); got != want {
			t.Errorf("isSafeIfaceName(%q) = %v, want %v", in, got, want)
		}
	}
}
