package localdns

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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

		dir := t.TempDir()
		path := filepath.Join(dir, "environment")
		if err := os.WriteFile(path, []byte("LOCALDNS_COREFILE_BASE=base\nLOCALDNS_COREFILE_WITH_HOSTS=encoded\nSHOULD_ENABLE_HOSTS_PLUGIN=false\nLOCALDNS_CRITICAL_FQDNS=mcr.microsoft.com\n"), 0o640); err != nil {
			t.Fatalf("os.WriteFile: %v", err)
		}
		var commands [][]string
		task := &configureTask{
			logger:                slog.New(slog.NewTextHandler(io.Discard, nil)),
			fqdn:                  "cluster.example.com",
			environmentPath:       path,
			hostsPath:             filepath.Join(dir, "hosts"),
			hostsSetupScriptPath:  createArtifact(t, dir, "aks-localdns-hosts-setup.sh", 0o755),
			hostsSetupServicePath: createArtifact(t, dir, "aks-localdns-hosts-setup.service", 0o644),
			hostsSetupTimerPath:   createArtifact(t, dir, "aks-localdns-hosts-setup.timer", 0o644),
			localdnsServicePath:   createArtifact(t, dir, "localdns.service", 0o644),
			localdnsScriptPath:    createArtifact(t, dir, "localdns.sh", 0o755),
			activeCorefilePath:    filepath.Join(dir, "updated.localdns.corefile"),
			runSystemctl: func(_ context.Context, args ...string) error {
				commands = append(commands, args)
				return nil
			},
		}

		if err := task.Do(context.Background()); err != nil {
			t.Fatalf("Do() error = %v", err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("os.ReadFile: %v", err)
		}
		want := "LOCALDNS_COREFILE_BASE=base\nLOCALDNS_COREFILE_WITH_HOSTS=encoded\nSHOULD_ENABLE_HOSTS_PLUGIN=true\nLOCALDNS_CRITICAL_FQDNS=mcr.microsoft.com,cluster.example.com\n"
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
		wantCommands := [][]string{
			{"start", hostsSetupServiceUnit},
			{"enable", "--now", hostsSetupTimerUnit},
			{"restart", localdnsServiceUnit},
		}
		if !slices.EqualFunc(commands, wantCommands, slices.Equal) {
			t.Fatalf("commands = %v, want %v", commands, wantCommands)
		}

		if err := os.WriteFile(task.activeCorefilePath, []byte("hosts /etc/localdns/hosts {\n"), 0o644); err != nil {
			t.Fatalf("os.WriteFile: %v", err)
		}
		if err := task.Do(context.Background()); err != nil {
			t.Fatalf("second Do() error = %v", err)
		}
		wantCommands = append(wantCommands,
			[]string{"start", hostsSetupServiceUnit},
			[]string{"enable", "--now", hostsSetupTimerUnit},
		)
		if !slices.EqualFunc(commands, wantCommands, slices.Equal) {
			t.Fatalf("commands after second Do = %v, want %v", commands, wantCommands)
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

	t.Run("does nothing without hosts plugin artifacts", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		path := filepath.Join(dir, "environment")
		environment := "LOCALDNS_COREFILE_BASE=base\nLOCALDNS_COREFILE_WITH_HOSTS=encoded\nLOCALDNS_CRITICAL_FQDNS=mcr.microsoft.com\n"
		if err := os.WriteFile(path, []byte(environment), 0o640); err != nil {
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
		if string(got) != environment {
			t.Fatalf("environment = %q, want %q", got, environment)
		}
	})
}

func createArtifact(t *testing.T, dir, name string, mode os.FileMode) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(name), mode); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	return path
}
