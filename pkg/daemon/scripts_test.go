package daemon

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/Azure/unbounded/pkg/agent/hostroot"
)

// scriptsDir holds the host scripts and the Butane template, which the daemon
// has to agree with but which do not compile into it.
const scriptsDir = "../../scripts"

// TestScriptsInstallUnderTheAgentHostRoot keeps the scripts' HOST_ROOT in step
// with the agent library. The scripts choose where the binary goes and what
// uninstall removes, and the agent resolves every path from hostroot.Path; if
// the two disagree, a host is installed where the agent never looks, or
// uninstall removes a directory that is not the agent's.
func TestScriptsInstallUnderTheAgentHostRoot(t *testing.T) {
	t.Parallel()

	hostRoot := regexp.MustCompile(`(?m)^(?:readonly )?HOST_ROOT="([^"]*)"$`)

	for _, name := range []string{"bootstrap.sh", "install.sh", "uninstall.sh"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			script, err := os.ReadFile(filepath.Join(scriptsDir, name))
			if err != nil {
				t.Fatal(err)
			}

			matches := hostRoot.FindAllStringSubmatch(string(script), -1)
			if len(matches) != 1 {
				t.Fatalf("want exactly one HOST_ROOT assignment, got %d", len(matches))
			}
			if got := matches[0][1]; got != hostroot.Path {
				t.Errorf("HOST_ROOT = %q, want hostroot.Path %q", got, hostroot.Path)
			}
		})
	}
}

// butaneConfig is the part of a Butane config the template test reads.
type butaneConfig struct {
	Variant string `json:"variant"`
	Version string `json:"version"`
	Storage struct {
		Directories []butaneNode `json:"directories"`
		Files       []butaneNode `json:"files"`
	} `json:"storage"`
	Systemd struct {
		Units []struct {
			Name     string `json:"name"`
			Enabled  *bool  `json:"enabled"`
			Contents string `json:"contents"`
		} `json:"units"`
	} `json:"systemd"`
}

type butaneNode struct {
	Path     string `json:"path"`
	Mode     int    `json:"mode"`
	Contents struct {
		Local string `json:"local"`
	} `json:"contents"`
}

// TestBootstrapButaneTemplate ties the documented Butane template for hosts
// provisioned by Ignition to the agent. Reset removes the first-boot unit by
// FirstBootUnitName, and the unit must stop running once the agent unit exists
// at ServiceUnitPath. Within the template, the unit has to run the files it
// writes, keep the ones that carry secrets private, and remove them once
// bootstrap succeeds. Rendering it is checked with Butane itself by hack/acl.
func TestBootstrapButaneTemplate(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join(scriptsDir, "aks-flex-node-bootstrap.bu"))
	if err != nil {
		t.Fatal(err)
	}

	var cfg butaneConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse template: %v", err)
	}

	if cfg.Variant != "flatcar" || cfg.Version != "1.1.0" {
		t.Errorf("variant %s %s, want flatcar 1.1.0, which renders Ignition 3.4.0", cfg.Variant, cfg.Version)
	}

	files := map[string]string{}
	for _, f := range cfg.Storage.Files {
		files[f.Contents.Local] = f.Path
		want := 0o600
		if f.Contents.Local == "bootstrap.sh" {
			want = 0o700
		}
		if f.Mode != want {
			t.Errorf("%s mode = %o, want %o", f.Path, f.Mode, want)
		}
	}
	for _, local := range []string{"bootstrap.sh", "bootstrap.env", "base-config.json"} {
		if files[local] == "" {
			t.Fatalf("the template does not write %s", local)
		}
	}

	firstBootDir := path.Dir(files["bootstrap.sh"])
	for _, local := range []string{"bootstrap.env", "base-config.json"} {
		if dir := path.Dir(files[local]); dir != firstBootDir {
			t.Errorf("%s is in %s, not %s, which the unit removes after bootstrap", local, dir, firstBootDir)
		}
	}
	if !slices.ContainsFunc(cfg.Storage.Directories, func(d butaneNode) bool {
		return d.Path == firstBootDir && d.Mode == 0o700
	}) {
		t.Errorf("the template does not create %s with mode 0700", firstBootDir)
	}

	if len(cfg.Systemd.Units) != 1 {
		t.Fatalf("want one unit, got %d", len(cfg.Systemd.Units))
	}
	unit := cfg.Systemd.Units[0]
	if unit.Name != FirstBootUnitName {
		t.Errorf("unit name = %q, want %q, which reset removes", unit.Name, FirstBootUnitName)
	}
	if unit.Enabled == nil || !*unit.Enabled {
		t.Error("the unit is not enabled, so it never runs")
	}

	lines := strings.Split(unit.Contents, "\n")
	for _, want := range []string{
		"ConditionPathExists=!" + ServiceUnitPath,
		"AssertPathExists=" + files["bootstrap.sh"],
		"Environment=AKS_FLEX_NODE_BASE_CONFIG_FILE=" + files["base-config.json"],
		"EnvironmentFile=" + files["bootstrap.env"],
		"ExecStart=/bin/bash " + files["bootstrap.sh"],
		"ExecStartPost=/bin/rm -rf " + firstBootDir,
		"Restart=on-failure",
		"WantedBy=multi-user.target",
	} {
		if !slices.Contains(lines, want) {
			t.Errorf("unit has no line %q:\n%s", want, unit.Contents)
		}
	}
}
