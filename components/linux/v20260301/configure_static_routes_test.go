package v20260301

import (
	"os"
	"path/filepath"
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
				`DEV="eth0"`,
				`GW="172.18.1.1"`,
				`ip -4 route replace 172.16.1.0/24 via "$GW" dev "$DEV"`,
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
				`DEV=$(resolve_default_dev)`,
				`GW=$(resolve_default_gw "$DEV")`,
				"cannot install route 172.16.2.0/24",
				`ip -4 route replace 172.16.2.0/24 via "$GW" dev "$DEV"`,
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
			name: "rejects IPv6 destination",
			routes: []*linux.StaticRoute{
				linux.StaticRoute_builder{Destination: to.Ptr("2001:db8::/32")}.Build(),
			},
			wantErr: true,
		},
		{
			name: "rejects IPv6 gateway",
			routes: []*linux.StaticRoute{
				linux.StaticRoute_builder{
					Destination: to.Ptr("172.16.1.0/24"),
					Gateway:     to.Ptr("2001:db8::1"),
				}.Build(),
			},
			wantErr: true,
		},
		{
			name: "auto-resolve fails hard when gateway never appears",
			routes: []*linux.StaticRoute{
				linux.StaticRoute_builder{Destination: to.Ptr("172.16.2.0/24")}.Build(),
			},
			wantContains: []string{
				"resolve_default_gw",
				"|| { echo",
				"exit 1",
			},
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

func TestValidateConfigureStaticRoutesSpec(t *testing.T) {
	t.Parallel()

	routes := []*linux.StaticRoute{
		linux.StaticRoute_builder{Destination: to.Ptr("172.16.1.0/24"), Gateway: to.Ptr("172.18.1.1")}.Build(),
	}

	tests := []struct {
		name    string
		spec    *linux.ConfigureStaticRoutesSpec
		wantErr bool
	}{
		{
			name:    "nil spec is rejected",
			spec:    nil,
			wantErr: true,
		},
		{
			name: "routes without explicit opt-in are rejected",
			spec: linux.ConfigureStaticRoutesSpec_builder{
				Routes: routes,
			}.Build(),
			wantErr: true,
		},
		{
			name: "routes with explicit opt-in are accepted",
			spec: linux.ConfigureStaticRoutesSpec_builder{
				Enabled: to.Ptr(true),
				Routes:  routes,
			}.Build(),
		},
		{
			name: "no routes with opt-in disabled is allowed",
			spec: linux.ConfigureStaticRoutesSpec_builder{
				Enabled: to.Ptr(false),
			}.Build(),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateConfigureStaticRoutesSpec(tc.spec)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got: %v", err)
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

func TestWriteScriptIfChanged(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "script.sh")

	// First write.
	changed, err := writeScriptIfChanged(path, []byte("hello"))
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	if !changed {
		t.Errorf("first write: expected changed=true")
	}

	// Identical content.
	info1, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	changed, err = writeScriptIfChanged(path, []byte("hello"))
	if err != nil {
		t.Fatalf("noop write: %v", err)
	}
	if changed {
		t.Errorf("noop write: expected changed=false")
	}
	info2, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !info1.ModTime().Equal(info2.ModTime()) {
		t.Errorf("noop write touched the file (mtime changed)")
	}

	// Different content.
	changed, err = writeScriptIfChanged(path, []byte("goodbye"))
	if err != nil {
		t.Fatalf("update write: %v", err)
	}
	if !changed {
		t.Errorf("update write: expected changed=true")
	}
	got, err := os.ReadFile(path) // #nosec G304 -- path is t.TempDir()-scoped
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "goodbye" {
		t.Errorf("file content = %q, want %q", got, "goodbye")
	}
}
