package arc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

func TestPreflightDisabled(t *testing.T) {
	t.Parallel()

	if got := Preflight(&config.Config{}, slog.Default()); len(got) != 0 {
		t.Fatalf("Preflight() returned %d checks, want 0", len(got))
	}
}

func TestConnectedMachineChecker(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		lookupErr     error
		serviceActive bool
		output        string
		outputErr     error
		wantSeverity  preflight.Severity
		wantMessage   string
	}{
		"connected": {
			serviceActive: true,
			output:        "Agent Status: Connected\n",
			wantSeverity:  preflight.SeverityOK,
			wantMessage:   "Azure Connected Machine agent and identity service are ready",
		},
		"agent missing": {
			lookupErr:    errors.New("not found"),
			wantSeverity: preflight.SeverityError,
			wantMessage:  "azcmagent is not installed",
		},
		"himds inactive": {
			serviceActive: false,
			wantSeverity:  preflight.SeverityError,
			wantMessage:   "Arc identity service himdsd is not active",
		},
		"show fails": {
			serviceActive: true,
			outputErr:     errors.New("show failed"),
			wantSeverity:  preflight.SeverityError,
			wantMessage:   "failed to inspect Arc connection status",
		},
		"disconnected": {
			serviceActive: true,
			output:        "Agent Status: Disconnected\n",
			wantSeverity:  preflight.SeverityError,
			wantMessage:   "Azure Connected Machine agent is not connected",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			checker := connectedMachineChecker{
				log: slog.New(slog.NewTextHandler(io.Discard, nil)),
				deps: connectedMachineCheckDeps{
					lookupPath:      func(string) (string, error) { return "/usr/bin/azcmagent", tt.lookupErr },
					isServiceActive: func(context.Context, *slog.Logger, string) bool { return tt.serviceActive },
					output: func(context.Context, *slog.Logger, string, ...string) (string, error) {
						return tt.output, tt.outputErr
					},
				},
			}
			results := checker.Check(t.Context())
			if len(results) != 1 {
				t.Fatalf("Check() returned %d results, want 1", len(results))
			}
			if results[0].Severity != tt.wantSeverity || results[0].Message != tt.wantMessage {
				t.Fatalf("Check() result = %#v, want severity %q message %q", results[0], tt.wantSeverity, tt.wantMessage)
			}
		})
	}
}
