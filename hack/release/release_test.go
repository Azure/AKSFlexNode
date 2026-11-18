package release

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	targetSHA = "0123456789abcdef0123456789abcdef01234567"
	otherSHA  = "abcdef0123456789abcdef0123456789abcdef01"
)

func validationScript(t *testing.T) string {
	t.Helper()

	data, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}

	var workflow struct {
		Jobs struct {
			Prepare struct {
				Steps []struct {
					ID  string `yaml:"id"`
					Run string `yaml:"run"`
				} `yaml:"steps"`
			} `yaml:"prepare"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatalf("parse release workflow: %v", err)
	}

	for _, step := range workflow.Jobs.Prepare.Steps {
		if step.ID == "release" && step.Run != "" {
			return step.Run
		}
	}

	t.Fatal("release validation step not found")
	return ""
}

func TestReleaseValidation(t *testing.T) {
	t.Parallel()

	script := validationScript(t)
	for _, tc := range []struct {
		name        string
		version     string
		nonMainRef  bool
		unreachable bool
		dryRun      bool
		badSHA      bool
		tagSHA      string
		changelog   string
		wantError   string
		wantWarning string
	}{
		{
			name:        "alpha integration publish",
			version:     "v0.4.0-alpha.1",
			nonMainRef:  true,
			unreachable: true,
			wantWarning: "Alpha release commit " + targetSHA + " is not reachable from main",
		},
		{
			name:        "alpha main dispatch nonmain target",
			version:     "v0.4.0-alpha.0",
			unreachable: true,
			wantWarning: "Alpha release commit " + targetSHA + " is not reachable from main",
		},
		{
			name:    "alpha main reachable",
			version: "v0.4.0-alpha.1",
		},
		{
			name:       "stable nonmain dispatch",
			version:    "v0.4.0",
			nonMainRef: true,
			wantError:  "Publishing runs must be dispatched from main",
		},
		{
			name:       "beta nonmain dispatch",
			version:    "v0.4.0-beta.1",
			nonMainRef: true,
			wantError:  "Publishing runs must be dispatched from main",
		},
		{
			name:       "rc nonmain dispatch",
			version:    "v0.4.0-rc.1",
			nonMainRef: true,
			wantError:  "Publishing runs must be dispatched from main",
		},
		{
			name:        "stable nonmain target",
			version:     "v0.4.0",
			unreachable: true,
			wantError:   "Release commit " + targetSHA + " is not reachable from main",
		},
		{
			name:        "beta nonmain target",
			version:     "v0.4.0-beta.1",
			unreachable: true,
			wantError:   "Release commit " + targetSHA + " is not reachable from main",
		},
		{
			name:        "rc nonmain target",
			version:     "v0.4.0-rc.1",
			unreachable: true,
			wantError:   "Release commit " + targetSHA + " is not reachable from main",
		},
		{
			name:    "stable main reachable",
			version: "v0.4.0",
		},
		{
			name:    "beta main reachable",
			version: "v0.4.0-beta.1",
		},
		{
			name:    "rc main reachable",
			version: "v0.4.0-rc.1",
		},
		{
			name:        "stable dry run nonmain",
			version:     "v0.4.0",
			nonMainRef:  true,
			unreachable: true,
			dryRun:      true,
			wantWarning: "Dry-run commit " + targetSHA + " is not reachable from main",
		},
		{
			name:        "alpha dry run nonmain",
			version:     "v0.4.0-alpha.1",
			nonMainRef:  true,
			unreachable: true,
			dryRun:      true,
			wantWarning: "Dry-run commit " + targetSHA + " is not reachable from main",
		},
		{
			name:        "rc dry run nonmain",
			version:     "v0.4.0-rc.1",
			nonMainRef:  true,
			unreachable: true,
			dryRun:      true,
			wantWarning: "Dry-run commit " + targetSHA + " is not reachable from main",
		},
		{
			name:       "invalid alpha leading zero",
			version:    "v0.4.0-alpha.01",
			nonMainRef: true,
			wantError:  "Invalid release version",
		},
		{
			name:       "invalid alpha suffix",
			version:    "v0.4.0-alpha.1.extra",
			nonMainRef: true,
			wantError:  "Invalid release version",
		},
		{
			name:       "invalid target sha",
			version:    "v0.4.0-alpha.1",
			nonMainRef: true,
			badSHA:     true,
			wantError:  "target_sha must be a full 40-character lowercase commit SHA",
		},
		{
			name:       "existing tag mismatch",
			version:    "v0.4.0-alpha.1",
			nonMainRef: true,
			tagSHA:     otherSHA,
			wantError:  "Existing tag v0.4.0-alpha.1 points to " + otherSHA,
		},
		{
			name:       "dry run existing tag mismatch",
			version:    "v0.4.0-alpha.1",
			nonMainRef: true,
			dryRun:     true,
			tagSHA:     otherSHA,
			wantError:  "Existing tag v0.4.0-alpha.1 points to " + otherSHA,
		},
		{
			name:       "existing tag matches",
			version:    "v0.4.0-alpha.1",
			nonMainRef: true,
			tagSHA:     targetSHA,
		},
		{
			name:       "alpha missing unreleased changelog",
			version:    "v0.4.0-alpha.1",
			nonMainRef: true,
			changelog:  "# Changelog\n",
			wantError:  "CHANGELOG.md does not contain an Unreleased section",
		},
		{
			name:      "stable missing dated changelog",
			version:   "v0.4.0",
			changelog: "## [Unreleased]\n",
			wantError: "CHANGELOG.md must contain exactly one dated 0.4.0 release section",
		},
		{
			name:      "stable invalid release date",
			version:   "v0.4.0",
			changelog: "## [0.4.0] - 2026-02-30\n",
			wantError: "CHANGELOG.md contains an invalid release date",
		},
		{
			name:      "stable missing comparison link",
			version:   "v0.4.0",
			changelog: "## [0.4.0] - 2026-01-30\n",
			wantError: "CHANGELOG.md must contain exactly one comparison link ending at v0.4.0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			git := `#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  rev-parse) [[ "$*" == 'rev-parse HEAD' ]]; echo "$FAKE_TARGET" ;;
  show-ref) [[ "$*" == "show-ref --verify --quiet refs/tags/$REQUESTED_VERSION" ]]; [[ -n "$FAKE_TAG_TARGET" ]] ;;
  rev-list) [[ "$*" == "rev-list -n 1 $REQUESTED_VERSION" ]]; echo "$FAKE_TAG_TARGET" ;;
  fetch) [[ "$*" == 'fetch --no-tags origin refs/heads/main:refs/remotes/origin/main' ]] ;;
  merge-base) [[ "$*" == "merge-base --is-ancestor $FAKE_TARGET refs/remotes/origin/main" ]]; [[ "$FAKE_REACHABLE" == true ]] ;;
  show) [[ "$*" == "show $FAKE_TARGET:CHANGELOG.md" ]]; printf '%s' "$FAKE_CHANGELOG" ;;
  *) echo "unexpected git command: $*" >&2; exit 2 ;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "git"), []byte(git), 0o755); err != nil {
				t.Fatal(err)
			}

			ref := "refs/heads/main"
			if tc.nonMainRef {
				ref = "refs/heads/hbc/isolation-stack-alpha"
			}

			requestedSHA := targetSHA
			if tc.badSHA {
				requestedSHA = "0123456"
			}

			changelog := tc.changelog
			if changelog == "" {
				changelog = "## [Unreleased]\n## [0.4.0] - 2026-01-30\n[0.4.0]: https://github.com/Azure/AKSFlexNode/compare/v0.3.0...v0.4.0\n"
			}

			outputPath := filepath.Join(dir, "output")
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", "-e", "-o", "pipefail", "-c", script)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(),
				"PATH="+dir+":"+os.Getenv("PATH"),
				"REQUESTED_VERSION="+tc.version,
				"REQUESTED_TARGET_SHA="+requestedSHA,
				fmt.Sprintf("DRY_RUN=%t", tc.dryRun),
				"GITHUB_REF="+ref,
				"GITHUB_REPOSITORY=Azure/AKSFlexNode",
				"GITHUB_OUTPUT="+outputPath,
				"GITHUB_STEP_SUMMARY="+filepath.Join(dir, "summary"),
				"FAKE_TARGET="+targetSHA,
				"FAKE_TAG_TARGET="+tc.tagSHA,
				fmt.Sprintf("FAKE_REACHABLE=%t", !tc.unreachable),
				"FAKE_CHANGELOG="+changelog,
			)
			output, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("validation timed out: %v\n%s", ctx.Err(), output)
			}

			if tc.wantError != "" {
				if err == nil || !strings.Contains(string(output), tc.wantError) {
					t.Fatalf("validation error = %v, output = %s; want %q", err, output, tc.wantError)
				}
				if _, err := os.Stat(outputPath); !os.IsNotExist(err) {
					t.Fatalf("failed validation wrote outputs: %v", err)
				}
				return
			}

			if err != nil {
				t.Fatalf("validation failed: %v\n%s", err, output)
			}
			if tc.wantWarning != "" && !strings.Contains(string(output), "::warning::"+tc.wantWarning) {
				t.Errorf("validation output = %s; want warning %q", output, tc.wantWarning)
			}

			got, err := os.ReadFile(outputPath)
			if err != nil {
				t.Fatal(err)
			}
			want := fmt.Sprintf("version=%s\ntarget_sha=%s\nprerelease=%t\npublish=%t\ntag_exists=%t\n",
				tc.version, targetSHA, strings.Contains(tc.version, "-"), !tc.dryRun, tc.tagSHA != "")
			if string(got) != want {
				t.Errorf("outputs = %q, want %q", got, want)
			}
		})
	}
}
