package agentdeployment

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"

	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/phases/host"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

func TestContainerized(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		value string
		want  bool
	}{
		"unset": {
			value: "",
			want:  false,
		},
		"true": {
			value: "true",
			want:  true,
		},
		"one": {
			value: "1",
			want:  true,
		},
		"false": {
			value: "false",
			want:  false,
		},
		"invalid": {
			value: "yes-please",
			want:  false,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			getenv := func(key string) string {
				if key == ContainerizedEnv {
					return tc.value
				}
				return ""
			}
			if got := containerized(getenv); got != tc.want {
				t.Errorf("containerized() with %s=%q = %v, want %v", ContainerizedEnv, tc.value, got, tc.want)
			}
		})
	}
}

func TestCredentialPlugin(t *testing.T) {
	t.Parallel()

	const (
		running = "/opt/aks-flex-node/bin/aks-flex-node"
		host    = "/opt/unbounded/agent/bin/aks-flex-node"
	)
	tests := map[string]struct {
		containerized bool
		executableErr error
		want          string
		wantErr       bool
	}{
		"a host runs the host's agent": {
			want: host,
		},
		"the container runs the running binary": {
			containerized: true,
			want:          running,
		},
		"the container fails without its own binary": {
			containerized: true,
			executableErr: errors.New("no executable"),
			wantErr:       true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			resolved := false
			executable := func() (string, error) {
				resolved = true
				if tc.executableErr != nil {
					return "", tc.executableErr
				}
				return running, nil
			}
			got, err := credentialPlugin(tc.containerized, executable, func() string { return host })
			if (err != nil) != tc.wantErr {
				t.Fatalf("credentialPlugin(containerized=%v) error = %v, want error %v", tc.containerized, err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("credentialPlugin(containerized=%v) = %q, want %q", tc.containerized, got, tc.want)
			}
			if resolved != tc.containerized {
				t.Errorf("credentialPlugin(containerized=%v) resolved the running binary: %v", tc.containerized, resolved)
			}
		})
	}
}

type namedCheck string

func (c namedCheck) Name() string                           { return string(c) }
func (namedCheck) Check(context.Context) []preflight.Result { return nil }

func TestPreflightChecksInAContainer(t *testing.T) {
	t.Parallel()

	all := []string{"privileged-user", "host-packages", "containerd-active", "swap-active"}
	tests := map[string]struct {
		containerized bool
		want          []string
	}{
		"on the host, every check": {
			containerized: false,
			want:          all,
		},
		"in a container, the ones that apply": {
			containerized: true,
			want:          []string{"privileged-user", "swap-active"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var checks []preflight.Checker
			for _, name := range all {
				checks = append(checks, namedCheck(name))
			}

			var got []string
			for _, check := range preflightChecks(checks, tc.containerized) {
				got = append(got, check.Name())
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("checks = %q, want %q", got, tc.want)
			}
			if len(checks) != len(all) || checks[1].Name() != "host-packages" {
				t.Errorf("preflightChecks changed its argument: %v", checks)
			}
		})
	}
}

// The checks a container leaves out are unbounded's; this notices if they're renamed.
func TestContainerSkippedPreflightChecksExist(t *testing.T) {
	t.Parallel()

	var names []string
	for _, check := range host.Preflight(slog.New(slog.DiscardHandler), agentconfig.AgentConfig{}, nil) {
		names = append(names, check.Name())
	}
	for _, skipped := range containerSkippedPreflightChecks {
		if !slices.Contains(names, skipped) {
			t.Errorf("unbounded's host preflight has no %q check (has %q)", skipped, names)
		}
	}
}
