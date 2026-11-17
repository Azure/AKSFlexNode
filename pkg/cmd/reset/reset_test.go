package reset

import (
	"errors"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/daemon"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestSelectResetMode(t *testing.T) {
	t.Parallel()

	corruptState := errors.New("daemon state checksum mismatch")
	for _, tc := range []struct {
		name          string
		state         *daemon.State
		loadErr       error
		requestedMode isolation.Mode
		want          isolation.Mode
		wantErr       string
		wantCause     error
	}{
		{
			name:  "healthy state selects the applied worker",
			state: &daemon.State{Isolation: isolation.ModeNone},
			want:  isolation.ModeNone,
		},
		{
			name:          "matching override",
			state:         &daemon.State{Isolation: isolation.ModeNone},
			requestedMode: isolation.ModeNone,
			want:          isolation.ModeNone,
		},
		{
			name:          "conflicting override cannot replace trusted state",
			state:         &daemon.State{Isolation: isolation.ModeNone},
			requestedMode: isolation.ModeNSpawn,
			wantErr:       "conflicts with applied worker isolation",
		},
		{
			name: "missing state leaves runtime discovery in control",
		},
		{
			name:          "missing state accepts explicit mode",
			requestedMode: isolation.ModeNone,
			want:          isolation.ModeNone,
		},
		{
			name:      "unreadable state requires explicit recovery selection",
			loadErr:   corruptState,
			wantErr:   "reset --isolation <mode>",
			wantCause: corruptState,
		},
		{
			name:          "none recovery ignores untrusted state contents",
			state:         &daemon.State{Isolation: isolation.ModeNSpawn},
			loadErr:       corruptState,
			requestedMode: isolation.ModeNone,
			want:          isolation.ModeNone,
		},
		{
			name:          "nspawn recovery ignores untrusted state contents",
			state:         &daemon.State{Isolation: isolation.ModeNone},
			loadErr:       corruptState,
			requestedMode: isolation.ModeNSpawn,
			want:          isolation.ModeNSpawn,
		},
		{
			name:          "OCI recovery ignores untrusted state contents",
			state:         &daemon.State{Isolation: isolation.ModeNone},
			loadErr:       corruptState,
			requestedMode: isolation.ModeContainer,
			want:          isolation.ModeContainer,
		},
		{
			name:          "healthy OCI state accepts matching override",
			state:         &daemon.State{Isolation: isolation.ModeContainer},
			requestedMode: isolation.ModeContainer,
			want:          isolation.ModeContainer,
		},
		{
			name:          "unknown override is rejected",
			requestedMode: "other",
			wantErr:       "invalid reset isolation",
		},
		{
			name:          "unreadable state cannot bypass override validation",
			loadErr:       corruptState,
			requestedMode: "other",
			wantErr:       "invalid reset isolation",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mode, err := selectResetMode(tc.state, tc.loadErr, tc.requestedMode)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("selectResetMode() error = %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("selectResetMode() error = %v, want %q", err, tc.wantErr)
			}
			if tc.wantCause != nil && !errors.Is(err, tc.wantCause) {
				t.Fatalf("selectResetMode() error = %v does not wrap %v", err, tc.wantCause)
			}
			if mode != tc.want {
				t.Fatalf("selectResetMode() mode = %q, want %q", mode, tc.want)
			}
		})
	}
}

func TestResetCommandHasExplicitIsolationOverride(t *testing.T) {
	t.Parallel()

	cmd := NewCommand()
	if err := cmd.ParseFlags([]string{"--isolation", "none"}); err != nil {
		t.Fatalf("ParseFlags() error = %v", err)
	}
	mode, err := cmd.Flags().GetString("isolation")
	if err != nil || mode != string(isolation.ModeNone) {
		t.Fatalf("isolation flag = %q, %v; want none", mode, err)
	}
}
