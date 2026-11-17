package agentruntime

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/isolation/container"
)

func TestContainerControllerUsesRuntimeStatusDuringActivation(t *testing.T) {
	t.Parallel()

	var calls []string
	controller := containerController{
		backend: &container.Backend{
			Run: func(_ context.Context, _ *slog.Logger, name string, args ...string) (string, error) {
				calls = append(calls, name+" "+strings.Join(args, " "))
				return `{"status":"running"}`, nil
			},
		},
	}
	running, detail, err := controller.Running(t.Context(), "kube2")
	if err != nil || !running || detail != "" {
		t.Fatalf("running=%t detail=%q error=%v", running, detail, err)
	}
	if len(calls) != 1 || !strings.Contains(calls[0], "state unbounded-kube2") {
		t.Fatalf("unexpected calls: %v", calls)
	}
}

func TestContainerLifecycleRejectsInvalidInputs(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		phase string
		slot  string
		want  string
	}{
		{
			name:  "invalid slot",
			phase: "pre-start",
			slot:  "kube3",
			want:  "invalid container slot",
		},
		{
			name:  "unknown phase",
			phase: "bad",
			slot:  "kube1",
			want:  "unknown node lifecycle phase",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := RunContainerLifecycle(t.Context(), slog.New(slog.DiscardHandler), tc.phase, tc.slot)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}
