package arc

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"
)

func TestAreArcServiceGroupsActive(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	tests := []struct {
		name           string
		activeServices map[string]bool
		want           bool
	}{
		{
			name: "legacy service active",
			activeServices: map[string]bool{
				"himdsd":       true,
				"gcarcservice": true,
				"extd":         true,
			},
			want: true,
		},
		{
			name: "renamed service active",
			activeServices: map[string]bool{
				"himdsd": true,
				"gcad":   true,
				"extd":   true,
			},
			want: true,
		},
		{
			name: "neither service active",
			activeServices: map[string]bool{
				"himdsd": true,
				"extd":   true,
			},
			want: false,
		},
		{
			name: "required service inactive",
			activeServices: map[string]bool{
				"gcad": true,
				"extd": true,
			},
			want: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := areArcServiceGroupsActive(ctx, logger, arcRequiredServiceGroups, func(_ context.Context, _ *slog.Logger, service string) bool {
				return tt.activeServices[service]
			})
			if got != tt.want {
				t.Fatalf("areArcServiceGroupsActive() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestArcServiceConstantsIncludeRenamedService(t *testing.T) {
	t.Parallel()

	const renamedService = "gcad"

	if !slices.Contains(arcServices, renamedService) {
		t.Fatalf("arcServices must include gcad for azcmagent v1.62+ cleanup")
	}
	if !slices.Contains(arcServiceFiles, "/lib/systemd/system/"+renamedService+".service") {
		t.Fatalf("arcServiceFiles must include /lib/systemd/system/gcad.service")
	}
	if !slices.Contains(arcServiceFiles, "/etc/systemd/system/"+renamedService+".service") {
		t.Fatalf("arcServiceFiles must include /etc/systemd/system/gcad.service")
	}
}
