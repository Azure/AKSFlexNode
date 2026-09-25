package ignition

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"k8s.io/utils/ptr"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/daemon"
	"github.com/Azure/AKSFlexNode/scripts"
)

// The Ignition types are written out rather than taken from github.com/coreos/ignition, which
// would bring in the whole specification for the few fields used here. The version is pinned and
// asserted by tests.
const specVersion = "3.4.0"

const (
	modeSecretDir  = 0o700
	modeSecretFile = 0o600
	modeScript     = 0o700

	// baseConfigPlaceholder is the line in bootstrap.sh that stands for the base config.
	baseConfigPlaceholder = "__AKS_FLEX_NODE_BASE_CONFIG_JSON__"
)

var (
	// Both directories are under the config directory, so reset removes them with it.
	firstBootDir   = path.Join(config.ConfigDir, "first-boot")
	bootstrapPath  = path.Join(firstBootDir, "bootstrap.sh")
	credentialsDir = path.Join(config.ConfigDir, "credentials")
)

type ignitionConfig struct {
	Ignition ignitionVersion `json:"ignition"`
	Storage  storage         `json:"storage"`
	Systemd  systemd         `json:"systemd"`
}

type ignitionVersion struct {
	Version string `json:"version"`
}

type storage struct {
	Directories []directory `json:"directories,omitempty"`
	Files       []file      `json:"files,omitempty"`
}

type directory struct {
	Path string `json:"path"`
	Mode int    `json:"mode"`
}

type file struct {
	Path      string   `json:"path"`
	Mode      int      `json:"mode"`
	Overwrite *bool    `json:"overwrite,omitempty"`
	Contents  contents `json:"contents"`
}

type contents struct {
	Source      string `json:"source"`
	Compression string `json:"compression,omitempty"`
}

type systemd struct {
	Units []unit `json:"units"`
}

type unit struct {
	Name     string `json:"name"`
	Enabled  *bool  `json:"enabled,omitempty"`
	Contents string `json:"contents"`
}

// credential is a file written to the host for bootstrap.sh to use, and the bootstrap.sh flag
// that points at it.
type credential struct {
	flag     string
	hostPath string
	content  []byte
}

// renderInput is everything the Ignition config is built from, already validated.
type renderInput struct {
	// baseConfig is compact JSON, or empty to leave bootstrap.sh without one.
	baseConfig    []byte
	credential    *credential
	bootstrapArgs []string
}

func render(in renderInput) ([]byte, error) {
	script, err := populateBootstrapScript(scripts.Bootstrap, in.baseConfig)
	if err != nil {
		return nil, err
	}
	scriptSource, err := gzipDataURL([]byte(script))
	if err != nil {
		return nil, err
	}

	args := append([]string(nil), in.bootstrapArgs...)
	cfg := ignitionConfig{
		Ignition: ignitionVersion{Version: specVersion},
		Storage: storage{
			Directories: []directory{{Path: firstBootDir, Mode: modeSecretDir}},
			Files: []file{{
				Path:      bootstrapPath,
				Mode:      modeScript,
				Overwrite: ptr.To(true),
				Contents:  contents{Source: scriptSource, Compression: "gzip"},
			}},
		},
	}
	if in.credential != nil {
		cfg.Storage.Directories = append(cfg.Storage.Directories, directory{Path: credentialsDir, Mode: modeSecretDir})
		cfg.Storage.Files = append(cfg.Storage.Files, file{
			Path:      in.credential.hostPath,
			Mode:      modeSecretFile,
			Overwrite: ptr.To(true),
			Contents:  contents{Source: dataURL(in.credential.content)},
		})
		args = append(args, in.credential.flag, in.credential.hostPath)
	}
	cfg.Systemd.Units = []unit{{
		Name:     daemon.FirstBootUnitName,
		Enabled:  ptr.To(true),
		Contents: firstBootUnit(args),
	}}

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode Ignition config: %w", err)
	}

	return append(out, '\n'), nil
}

// populateBootstrapScript puts the base config in place of the placeholder, the way a published
// script is populated. Without a base config the placeholder stays, and bootstrap.sh starts from
// an empty config and fetches bootstrap data.
func populateBootstrapScript(script string, baseConfig []byte) (string, error) {
	if n := strings.Count(script, baseConfigPlaceholder); n != 1 {
		return "", fmt.Errorf("bootstrap.sh has %d base config placeholders, want 1", n)
	}
	if len(baseConfig) == 0 {
		return script, nil
	}
	// The placeholder sits in a quoted heredoc, so the JSON is taken literally. It must stay on
	// one line so that it cannot end the heredoc early.
	if bytes.ContainsAny(baseConfig, "\r\n") {
		return "", fmt.Errorf("base config must be compact JSON")
	}

	return strings.Replace(script, baseConfigPlaceholder, string(baseConfig), 1), nil
}

// firstBootUnit returns the unit that runs bootstrap.sh until the agent is installed.
func firstBootUnit(args []string) string {
	execStart := make([]string, 0, len(args)+2)
	execStart = append(execStart, "/bin/bash", bootstrapPath)
	for _, arg := range args {
		execStart = append(execStart, systemdQuote(arg))
	}

	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=Bootstrap AKS Flex Node\n")
	b.WriteString("Wants=network-online.target\n")
	b.WriteString("After=network-online.target nss-lookup.target\n")
	// Once the agent unit exists the agent runs the node, and bootstrap.sh would fail its
	// preflight check for an existing deployment. Skipping on that condition, rather than on a
	// marker of our own, also means a failure after the agent is installed is not retried.
	b.WriteString("ConditionPathExists=!" + daemon.ServiceUnitPath + "\n")
	// An assertion rather than a condition: a missing script is a provisioning error and should
	// fail the unit where it can be seen, not skip it.
	b.WriteString("AssertPathExists=" + bootstrapPath + "\n")
	// Retry for as long as it takes; the backoff below bounds the rate.
	b.WriteString("StartLimitIntervalSec=0\n\n")
	b.WriteString("[Service]\n")
	b.WriteString("Type=oneshot\n")
	b.WriteString("RemainAfterExit=yes\n")
	// Early boot failures, such as DNS not answering yet when network-online.target is reached,
	// are retried. The delay grows from 10s to 5min. systemd older than 254 ignores RestartSteps
	// and RestartMaxDelaySec and keeps the fixed delay.
	b.WriteString("Restart=on-failure\n")
	b.WriteString("RestartSec=10s\n")
	b.WriteString("RestartSteps=10\n")
	b.WriteString("RestartMaxDelaySec=300\n")
	b.WriteString("ExecStart=" + strings.Join(execStart, " ") + "\n")
	// The script carries the base config, including any bootstrap token. The installed config
	// has what the agent needs, so the copy goes once bootstrap succeeds.
	b.WriteString("ExecStartPost=/bin/rm -f " + bootstrapPath + "\n\n")
	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=multi-user.target\n")

	return b.String()
}

// systemdQuote quotes one argument of an ExecStart line. systemd expands % specifiers and $
// variables even inside double quotes, so both are doubled. Control characters are rejected by
// validation before this is reached.
func systemdQuote(arg string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range arg {
		switch r {
		case '\\', '"':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '%':
			b.WriteString("%%")
		case '$':
			b.WriteString("$$")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')

	return b.String()
}

func dataURL(content []byte) string {
	return "data:;base64," + base64.StdEncoding.EncodeToString(content)
}

func gzipDataURL(content []byte) (string, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(content); err != nil {
		return "", fmt.Errorf("compress bootstrap.sh: %w", err)
	}
	if err := zw.Close(); err != nil {
		return "", fmt.Errorf("compress bootstrap.sh: %w", err)
	}

	return dataURL(buf.Bytes()), nil
}
