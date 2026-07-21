package localdns

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestAddCriticalFQDN(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		environment string
		fqdn        string
		want        string
		wantChanged bool
	}{
		{
			name:        "appends to existing list",
			environment: "OTHER=value\nLOCALDNS_CRITICAL_FQDNS=mcr.microsoft.com\n",
			fqdn:        "cluster.example.com",
			want:        "OTHER=value\nLOCALDNS_CRITICAL_FQDNS=mcr.microsoft.com,cluster.example.com\n",
			wantChanged: true,
		},
		{
			name:        "populates empty list",
			environment: "LOCALDNS_CRITICAL_FQDNS=\n",
			fqdn:        "cluster.example.com",
			want:        "LOCALDNS_CRITICAL_FQDNS=cluster.example.com\n",
			wantChanged: true,
		},
		{
			name:        "adds missing variable",
			environment: "OTHER=value\n",
			fqdn:        "cluster.example.com",
			want:        "OTHER=value\nLOCALDNS_CRITICAL_FQDNS=cluster.example.com\n",
			wantChanged: true,
		},
		{
			name:        "keeps existing fqdn",
			environment: "LOCALDNS_CRITICAL_FQDNS=mcr.microsoft.com,cluster.example.com\n",
			fqdn:        "cluster.example.com",
			want:        "LOCALDNS_CRITICAL_FQDNS=mcr.microsoft.com,cluster.example.com\n",
			wantChanged: false,
		},
		{
			name:        "ignores empty fqdn",
			environment: "OTHER=value\n",
			want:        "OTHER=value\n",
			wantChanged: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, changed := addCriticalFQDN(tt.environment, tt.fqdn)
			if got != tt.want {
				t.Fatalf("addCriticalFQDN() = %q, want %q", got, tt.want)
			}
			if changed != tt.wantChanged {
				t.Fatalf("addCriticalFQDN() changed = %t, want %t", changed, tt.wantChanged)
			}
		})
	}
}

func TestFQDNHost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "hostname", value: "cluster.example.com", want: "cluster.example.com"},
		{name: "hostname with port", value: "cluster.example.com:6443", want: "cluster.example.com"},
		{name: "URL", value: "https://cluster.example.com:443", want: "cluster.example.com"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := fqdnHost(tt.value); got != tt.want {
				t.Fatalf("fqdnHost(%q) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}

func TestConfigureTask(t *testing.T) {
	t.Parallel()

	t.Run("updates existing localdns environment", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "environment")
		if err := os.WriteFile(path, []byte("LOCALDNS_CRITICAL_FQDNS=mcr.microsoft.com\n"), 0o640); err != nil {
			t.Fatalf("os.WriteFile: %v", err)
		}
		task := &configureTask{
			logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
			fqdn:            "cluster.example.com",
			environmentPath: path,
		}

		if err := task.Do(context.Background()); err != nil {
			t.Fatalf("Do() error = %v", err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("os.ReadFile: %v", err)
		}
		want := "LOCALDNS_CRITICAL_FQDNS=mcr.microsoft.com,cluster.example.com\n"
		if string(got) != want {
			t.Fatalf("environment = %q, want %q", got, want)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("os.Stat: %v", err)
		}
		if info.Mode().Perm() != 0o640 {
			t.Fatalf("mode = %o, want 640", info.Mode().Perm())
		}
	})

	t.Run("does nothing when localdns is absent", func(t *testing.T) {
		t.Parallel()

		task := &configureTask{
			logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
			fqdn:            "cluster.example.com",
			environmentPath: filepath.Join(t.TempDir(), "missing"),
		}
		if err := task.Do(context.Background()); err != nil {
			t.Fatalf("Do() error = %v", err)
		}
	})
}
