package ignition

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Azure/AKSFlexNode/pkg/daemon"
	"github.com/Azure/AKSFlexNode/scripts"
)

var msiArgs = []string{"--auth", "msi", "--agent-version", "v0.1.0", "--fetch-bootstrap-data"}

// TestBootstrapFlagsMatchTheScript keeps the pass-through validation in step with bootstrap.sh, so
// an option added there is not refused here, and one removed there is not accepted here.
func TestBootstrapFlagsMatchTheScript(t *testing.T) {
	t.Parallel()

	valueArm := regexp.MustCompile(`(?m)^([ \t]*)(--auth\|[a-z0-9|-]+)\)[ \t]*$`).FindStringSubmatch(scripts.Bootstrap)
	if valueArm == nil {
		t.Fatal("bootstrap.sh no longer lists its value options in one case arm starting with --auth")
	}
	scriptValueFlags := strings.Split(valueArm[2], "|")

	// Switches are the other arms of the same case statement, so at the same indentation. Deeper
	// arms belong to the case that assigns the values.
	var scriptSwitches []string
	switchArm := regexp.MustCompile(`(?m)^` + valueArm[1] + `(--[a-z][a-z0-9-]*)\)[ \t]*$`)
	for _, m := range switchArm.FindAllStringSubmatch(scripts.Bootstrap, -1) {
		scriptSwitches = append(scriptSwitches, m[1])
	}

	for _, tt := range []struct {
		name         string
		script, here []string
	}{
		{name: "value options", script: scriptValueFlags, here: bootstrapValueFlags},
		{name: "switches", script: scriptSwitches, here: bootstrapSwitches},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			script, here := slices.Sorted(slices.Values(tt.script)), slices.Sorted(slices.Values(tt.here))
			if !slices.Equal(script, here) {
				t.Fatalf("bootstrap.sh has %q, the ignition command has %q", script, here)
			}
		})
	}
}

func TestOwnedBootstrapFlagsAreScriptFlags(t *testing.T) {
	t.Parallel()

	for flag := range ownedBootstrapFlags {
		if !slices.Contains(bootstrapValueFlags, flag) {
			t.Errorf("owned flag %s is not a bootstrap.sh value option", flag)
		}
	}
}

func TestValidateBootstrapArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		args     []string
		wantErr  string
		wantSeen []string
	}{
		{
			name:     "value options and switches",
			args:     []string{"--auth", "msi", "--fetch-bootstrap-data", "--config-overrides", `{"node":{"labels":{"a":"b"}}}`},
			wantSeen: []string{"--auth", "--config-overrides", "--fetch-bootstrap-data"},
		},
		{
			name:     "a value that looks like an option is taken as the value, as bootstrap.sh does",
			args:     []string{"--agent-pool-name", "--auth"},
			wantSeen: []string{"--agent-pool-name"},
		},
		{name: "unknown option", args: []string{"--nope"}, wantErr: `unknown bootstrap.sh option "--nope"`},
		{name: "help is not passed through", args: []string{"--help"}, wantErr: "unknown bootstrap.sh option"},
		{name: "positional argument", args: []string{"msi"}, wantErr: `unknown bootstrap.sh option "msi"`},
		{name: "missing value", args: []string{"--auth"}, wantErr: "--auth requires a value"},
		{name: "empty value", args: []string{"--auth", "", "--fetch-bootstrap-data"}, wantErr: "--auth requires a value"},
		{name: "newline in value", args: []string{"--config-overrides", "{\n}"}, wantErr: "control character"},
		{name: "host prefix is not an option", args: []string{"--host-prefix", "/opt/x"}, wantErr: `unknown bootstrap.sh option "--host-prefix"`},
		{name: "install dir is owned", args: []string{"--install-dir", "/opt/x/bin"}, wantErr: "cannot be passed through"},
		{name: "config path is owned", args: []string{"--config-path", "/etc/x.json"}, wantErr: "cannot be passed through"},
		{name: "secret file is owned", args: []string{"--sp-client-secret-file", "/etc/s"}, wantErr: "writes the file to the host"},
		{name: "certificate file is owned", args: []string{"--sp-client-certificate-file", "/etc/c"}, wantErr: "writes the file to the host"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			seen, err := validateBootstrapArgs(tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("validateBootstrapArgs() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateBootstrapArgs() error = %v", err)
			}
			var got []string
			for flag := range seen {
				got = append(got, flag)
			}
			if slices.Sort(got); !slices.Equal(got, tt.wantSeen) {
				t.Fatalf("seen = %q, want %q", got, tt.wantSeen)
			}
		})
	}
}

func TestBuildRenderInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		baseConfig string
		args       []string
		wantErr    string
		wantBase   string
	}{
		{name: "bootstrap data only", args: msiArgs},
		{
			name:       "base config is compacted",
			baseConfig: "{\n  \"agent\": {\"logLevel\": \"debug\"}\n}\n",
			args:       []string{"--agent-url", "https://example.com/a.tar.gz"},
			wantBase:   `{"agent":{"logLevel":"debug"}}`,
		},
		{
			name:    "an agent source is required",
			args:    []string{"--auth", "msi", "--fetch-bootstrap-data"},
			wantErr: "--agent-url or --agent-version",
		},
		{
			name:    "without a base config bootstrap data must be fetched",
			args:    []string{"--auth", "msi", "--agent-version", "v0.1.0"},
			wantErr: "needs --fetch-bootstrap-data",
		},
		{name: "base config must be an object", baseConfig: `["a"]`, args: msiArgs, wantErr: "must be a JSON object"},
		{name: "base config must not be null", baseConfig: `null`, args: msiArgs, wantErr: "must be a JSON object"},
		{name: "base config must be JSON", baseConfig: `{"a":`, args: msiArgs, wantErr: "must be a JSON object"},
		{name: "bad pass-through fails before anything is read", baseConfig: `{"a":`, args: []string{"--nope"}, wantErr: "unknown bootstrap.sh option"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var opts options
			if tt.baseConfig != "" {
				opts.baseConfigPath = filepath.Join(t.TempDir(), "base.json")
				if err := os.WriteFile(opts.baseConfigPath, []byte(tt.baseConfig), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			in, err := buildRenderInput(opts, tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("buildRenderInput() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildRenderInput() error = %v", err)
			}
			if string(in.baseConfig) != tt.wantBase {
				t.Errorf("baseConfig = %q, want %q", in.baseConfig, tt.wantBase)
			}
			if !slices.Equal(in.bootstrapArgs, tt.args) {
				t.Errorf("bootstrapArgs = %q, want %q", in.bootstrapArgs, tt.args)
			}
		})
	}
}

func TestLoadCredential(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		secret       bool
		fileName     string
		write        func(t *testing.T, path string)
		mode         os.FileMode
		wantErr      string
		wantFlag     string
		wantHostPath string
	}{
		{
			name:         "client secret",
			secret:       true,
			fileName:     "secret",
			write:        writeFile("s3cret\n"),
			mode:         0o600,
			wantFlag:     "--sp-client-secret-file",
			wantHostPath: "/etc/aks-flex-node/credentials/sp-client-secret",
		},
		{name: "readable secret is refused", secret: true, fileName: "secret", write: writeFile("s3cret"), mode: 0o644, wantErr: "group or other"},
		{name: "empty secret is refused", secret: true, fileName: "secret", write: writeFile(" \n"), mode: 0o600, wantErr: "empty"},
		{
			name:         "client certificate",
			fileName:     "client.pem",
			write:        writeTestClientCertificate,
			mode:         0o600,
			wantFlag:     "--sp-client-certificate-file",
			wantHostPath: "/etc/aks-flex-node/credentials/sp-client-certificate",
		},
		{
			name:         "pfx suffix is kept because PKCS#12 is recognized by it",
			fileName:     "client.PFX",
			write:        writeTestClientCertificate,
			mode:         0o600,
			wantFlag:     "--sp-client-certificate-file",
			wantHostPath: "/etc/aks-flex-node/credentials/sp-client-certificate.pfx",
		},
		{name: "invalid certificate is refused", fileName: "client.pem", write: writeFile("not a certificate"), mode: 0o600, wantErr: "certificate"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			localPath := filepath.Join(t.TempDir(), tt.fileName)
			tt.write(t, localPath)
			if err := os.Chmod(localPath, tt.mode); err != nil {
				t.Fatal(err)
			}
			var opts options
			if tt.secret {
				opts.spClientSecretFile = localPath
			} else {
				opts.spClientCertificateFile = localPath
			}

			cred, err := loadCredential(opts)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("loadCredential() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadCredential() error = %v", err)
			}
			want, err := os.ReadFile(localPath)
			if err != nil {
				t.Fatal(err)
			}
			if cred.flag != tt.wantFlag || cred.hostPath != tt.wantHostPath || !bytes.Equal(cred.content, want) {
				t.Fatalf("credential = {%s %s %d bytes}, want {%s %s %d bytes}",
					cred.flag, cred.hostPath, len(cred.content), tt.wantFlag, tt.wantHostPath, len(want))
			}
		})
	}
}

func TestLoadCredentialWithoutOne(t *testing.T) {
	t.Parallel()

	cred, err := loadCredential(options{})
	if err != nil || cred != nil {
		t.Fatalf("loadCredential() = %v, %v, want nil, nil", cred, err)
	}
}

func TestRender(t *testing.T) {
	t.Parallel()

	secret := &credential{
		flag:     "--sp-client-secret-file",
		hostPath: "/etc/aks-flex-node/credentials/sp-client-secret",
		content:  []byte("s3cret\n"),
	}

	tests := []struct {
		name       string
		in         renderInput
		wantScript string
	}{
		{
			name:       "without a base config the script is written as is",
			in:         renderInput{bootstrapArgs: msiArgs},
			wantScript: scripts.Bootstrap,
		},
		{
			name: "base config and credential",
			in: renderInput{
				baseConfig:    []byte(`{"azure":{"bootstrapToken":{"token":"abcdef.0123456789abcdef"}}}`),
				credential:    secret,
				bootstrapArgs: []string{"--auth", "service-principal", "--agent-version", "v0.1.0"},
			},
			wantScript: strings.Replace(scripts.Bootstrap, baseConfigPlaceholder,
				`{"azure":{"bootstrapToken":{"token":"abcdef.0123456789abcdef"}}}`, 1),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out, err := render(tt.in)
			if err != nil {
				t.Fatalf("render() error = %v", err)
			}
			var cfg ignitionConfig
			if err := json.Unmarshal(out, &cfg); err != nil {
				t.Fatalf("output is not JSON: %v", err)
			}
			if cfg.Ignition.Version != "3.4.0" {
				t.Errorf("version = %q, want 3.4.0", cfg.Ignition.Version)
			}

			wantDirs := []directory{{Path: "/etc/aks-flex-node/first-boot", Mode: 0o700}}
			if tt.in.credential != nil {
				wantDirs = append(wantDirs, directory{Path: "/etc/aks-flex-node/credentials", Mode: 0o700})
			}
			if !slices.Equal(cfg.Storage.Directories, wantDirs) {
				t.Errorf("directories = %+v, want %+v", cfg.Storage.Directories, wantDirs)
			}

			wantFiles := 1
			if tt.in.credential != nil {
				wantFiles = 2
			}
			if len(cfg.Storage.Files) != wantFiles {
				t.Fatalf("got %d files, want %d", len(cfg.Storage.Files), wantFiles)
			}
			script := cfg.Storage.Files[0]
			if script.Path != "/etc/aks-flex-node/first-boot/bootstrap.sh" || script.Mode != 0o700 ||
				script.Contents.Compression != "gzip" || script.Overwrite == nil || !*script.Overwrite {
				t.Errorf("script entry = %+v", script)
			}
			if got := decodeSource(t, script.Contents.Source, true); got != tt.wantScript {
				t.Errorf("script content differs from the embedded script with the base config in place")
			}

			wantArgs := append([]string(nil), tt.in.bootstrapArgs...)
			if tt.in.credential != nil {
				cred := cfg.Storage.Files[1]
				if cred.Path != tt.in.credential.hostPath || cred.Mode != 0o600 || cred.Contents.Compression != "" {
					t.Errorf("credential entry = %+v", cred)
				}
				if got := decodeSource(t, cred.Contents.Source, false); got != string(tt.in.credential.content) {
					t.Errorf("credential content = %q, want %q", got, tt.in.credential.content)
				}
				wantArgs = append(wantArgs, tt.in.credential.flag, tt.in.credential.hostPath)
			}

			if len(cfg.Systemd.Units) != 1 {
				t.Fatalf("got %d units, want 1", len(cfg.Systemd.Units))
			}
			u := cfg.Systemd.Units[0]
			if u.Name != daemon.FirstBootUnitName || u.Enabled == nil || !*u.Enabled {
				t.Errorf("unit = %s enabled=%v, want %s enabled", u.Name, u.Enabled, daemon.FirstBootUnitName)
			}
			if u.Contents != firstBootUnit(wantArgs) {
				t.Errorf("unit contents do not run bootstrap.sh with %q:\n%s", wantArgs, u.Contents)
			}
		})
	}
}

// TestRenderedModesAreDecimal pins the encoding: Ignition takes modes as JSON integers.
func TestRenderedModesAreDecimal(t *testing.T) {
	t.Parallel()

	out, err := render(renderInput{bootstrapArgs: msiArgs})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"mode": 448`, `"version": "3.4.0"`} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("output does not contain %s", want)
		}
	}
}

func TestFirstBootUnit(t *testing.T) {
	t.Parallel()

	want := `[Unit]
Description=Bootstrap AKS Flex Node
Wants=network-online.target
After=network-online.target nss-lookup.target
ConditionPathExists=!/etc/systemd/system/aks-flex-node-agent.service
AssertPathExists=/etc/aks-flex-node/first-boot/bootstrap.sh
StartLimitIntervalSec=0

[Service]
Type=oneshot
RemainAfterExit=yes
Restart=on-failure
RestartSec=10s
RestartSteps=10
RestartMaxDelaySec=300
ExecStart=/bin/bash /etc/aks-flex-node/first-boot/bootstrap.sh "--auth" "msi" "--config-overrides" "{\"a\":\"$$HOME %%h\"}"
ExecStartPost=/bin/rm -f /etc/aks-flex-node/first-boot/bootstrap.sh

[Install]
WantedBy=multi-user.target
`
	got := firstBootUnit([]string{"--auth", "msi", "--config-overrides", `{"a":"$HOME %h"}`})
	if got != want {
		t.Fatalf("firstBootUnit() =\n%s\nwant\n%s", got, want)
	}
}

func TestSystemdQuote(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, arg, want string
	}{
		{name: "plain", arg: "msi", want: `"msi"`},
		{name: "spaces stay in one argument", arg: "a b", want: `"a b"`},
		{name: "double quote", arg: `say "hi"`, want: `"say \"hi\""`},
		{name: "backslash", arg: `a\b`, want: `"a\\b"`},
		{name: "variable is not expanded", arg: "$HOME${X}", want: `"$$HOME$${X}"`},
		{name: "specifier is not expanded", arg: "%h%%", want: `"%%h%%%%"`},
		{name: "single quote needs nothing", arg: "it's", want: `"it's"`},
		{name: "empty", arg: "", want: `""`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := systemdQuote(tt.arg); got != tt.want {
				t.Fatalf("systemdQuote(%q) = %s, want %s", tt.arg, got, tt.want)
			}
		})
	}
}

func TestPopulateBootstrapScript(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, script, base, want, wantErr string
	}{
		{name: "replaces the placeholder", script: "a\n" + baseConfigPlaceholder + "\nb\n", base: `{"x":1}`, want: "a\n{\"x\":1}\nb\n"},
		{name: "no base config keeps the placeholder", script: "a\n" + baseConfigPlaceholder + "\n", want: "a\n" + baseConfigPlaceholder + "\n"},
		{name: "missing placeholder", script: "a\n", base: `{}`, wantErr: "0 base config placeholders"},
		{name: "two placeholders", script: baseConfigPlaceholder + baseConfigPlaceholder, base: `{}`, wantErr: "2 base config placeholders"},
		{name: "multi-line base config", script: baseConfigPlaceholder, base: "{\n}", wantErr: "compact"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := populateBootstrapScript(tt.script, []byte(tt.base))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("populateBootstrapScript() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("populateBootstrapScript() = %q, %v, want %q", got, err, tt.want)
			}
		})
	}
}

// TestPopulatedScriptReturnsTheBaseConfig runs the populated script's own reader, so the
// substitution is checked against how bash parses the heredoc rather than against a string.
func TestPopulatedScriptReturnsTheBaseConfig(t *testing.T) {
	t.Parallel()

	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not available")
	}

	tests := []struct {
		name string
		base string
	}{
		{name: "plain", base: `{"agent":{"logLevel":"info"}}`},
		{name: "shell syntax stays literal", base: `{"a":"$HOME $(id) ` + "`id`" + ` 'q' \\ \"d\""}`},
		{name: "heredoc delimiter inside a value", base: `{"a":"AKS_FLEX_NODE_EMBEDDED_CONFIG"}`},
		{name: "unicode", base: `{"a":"caf\u00e9 ` + "\u00e9" + `"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var compact bytes.Buffer
			if err := json.Compact(&compact, []byte(tt.base)); err != nil {
				t.Fatalf("test base config is not JSON: %v", err)
			}
			script, err := populateBootstrapScript(scripts.Bootstrap, compact.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			scriptPath := filepath.Join(t.TempDir(), "bootstrap.sh")
			if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
				t.Fatal(err)
			}

			// Sourcing defines the functions without running main.
			out, err := exec.CommandContext(t.Context(), bash, "-c", `source "$1" && write_embedded_base_config`, "bash", scriptPath).Output()
			if err != nil {
				t.Fatalf("reading the base config back failed: %v", err)
			}
			if got := strings.TrimSuffix(string(out), "\n"); got != compact.String() {
				t.Fatalf("base config read back = %s, want %s", got, compact.String())
			}
		})
	}
}

func TestWriteOutput(t *testing.T) {
	t.Parallel()

	t.Run("stdout", func(t *testing.T) {
		t.Parallel()

		var stdout bytes.Buffer
		if err := writeOutput(&stdout, "-", []byte("config")); err != nil || stdout.String() != "config" {
			t.Fatalf("writeOutput() = %v, stdout %q", err, stdout.String())
		}
	})

	t.Run("new file is private", func(t *testing.T) {
		t.Parallel()

		outputPath := filepath.Join(t.TempDir(), "node.ign")
		if err := writeOutput(io.Discard, outputPath, []byte("config")); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(outputPath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %o, want 600", info.Mode().Perm())
		}
	})

	t.Run("existing file is not reused", func(t *testing.T) {
		t.Parallel()

		outputPath := filepath.Join(t.TempDir(), "node.ign")
		if err := os.WriteFile(outputPath, []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := writeOutput(io.Discard, outputPath, []byte("config")); err == nil {
			t.Fatal("writeOutput() reused an existing file")
		}
		if got, _ := os.ReadFile(outputPath); string(got) != "old" {
			t.Fatalf("existing file was changed to %q", got)
		}
	})
}

func TestCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "bootstrap.sh options after --", args: append([]string{"--"}, msiArgs...)},
		{name: "bootstrap.sh options before --", args: append([]string{"--auth"}, msiArgs...), wantErr: "unknown flag"},
		{name: "positional arguments before --", args: append([]string{"msi", "--"}, msiArgs...), wantErr: "after --"},
		{name: "no bootstrap.sh options", args: nil, wantErr: "--agent-url or --agent-version"},
		{
			name:    "one credential at a time",
			args:    append([]string{"--sp-client-secret-file", "a", "--sp-client-certificate-file", "b", "--"}, msiArgs...),
			wantErr: "none of the others can be",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cmd := NewCommand()
			var stdout bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(tt.args)

			err := cmd.ExecuteContext(t.Context())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Execute() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			var cfg ignitionConfig
			if err := json.Unmarshal(stdout.Bytes(), &cfg); err != nil || len(cfg.Systemd.Units) != 1 {
				t.Fatalf("stdout is not the rendered config: %v", err)
			}
		})
	}
}

func decodeSource(t *testing.T, source string, gzipped bool) string {
	t.Helper()

	encoded, ok := strings.CutPrefix(source, "data:;base64,")
	if !ok {
		t.Fatalf("source %.40q is not a base64 data URL", source)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode data URL: %v", err)
	}
	if !gzipped {
		return string(raw)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}

	return string(out)
}

func writeFile(content string) func(t *testing.T, path string) {
	return func(t *testing.T, path string) {
		t.Helper()

		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func writeTestClientCertificate(t *testing.T, path string) {
	t.Helper()

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	certificate, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatalf("x509.CreateCertificate: %v", err)
	}
	privateKeyData, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("x509.MarshalPKCS8PrivateKey: %v", err)
	}
	data := append(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKeyData})...,
	)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
}
