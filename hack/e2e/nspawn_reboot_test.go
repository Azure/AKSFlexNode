package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNspawnRebootFingerprints(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		failure string
		success bool
	}{
		{name: "active", success: true},
		{name: "machine stopped", failure: "machine"},
		{name: "kubelet stopped", failure: "kubelet"},
		{name: "containerd stopped", failure: "containerd"},
		{name: "config missing", failure: "config"},
		{name: "certificate missing", failure: "certificate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			files := map[string]string{
				"state.json":  `{"activeMachine":"kube2","appliedSettingsVersion":"settings","nodeConfig":{"token":"fixture-secret"}}`,
				"config":      "kubelet config\n",
				"certificate": "fixture-secret\n",
				"agent.json":  "agent fixture-secret\n",
				"daemon.pem":  "daemon fixture-secret\n",
				"sudo": `#!/usr/bin/env bash
exec "$@"
`,
				"machinectl": `#!/usr/bin/env bash
set -euo pipefail
[[ "$*" == 'show kube2 --property=State --value' ]]
if [[ "$FAIL" == machine ]]; then
  echo stopped
else
  echo running
fi
`,
				"systemd-run": `#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == --machine=kube2 && "$2" == --quiet && "$3" == --pipe && "$4" == --wait ]]
shift 4
wc -c >> "$ROOT/reads"
case "$1:$2" in
  systemctl:is-active)
    [[ "$3" == --quiet ]]
    case "$4" in
      kubelet.service) [[ "$FAIL" != kubelet ]] ;;
      containerd.service) [[ "$FAIL" != containerd ]] ;;
      *) exit 1 ;;
    esac ;;
  /usr/local/bin/kubelet:--version) echo 'Kubernetes v1.35.0' ;;
  sha256sum:/var/lib/kubelet/config.yaml)
    [[ "$FAIL" != config ]]
    sha256sum "$ROOT/config" ;;
  sha256sum:/var/lib/kubelet/pki/kubelet-client-current.pem)
    [[ "$FAIL" != certificate ]]
    sha256sum "$ROOT/certificate" ;;
  *) exit 1 ;;
esac
`,
			}
			for name, content := range files {
				if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o700); err != nil {
					t.Fatal(err)
				}
			}

			output := runShellResult(t, []string{
				"SCRIPT=" + isolationScript(t),
				"ROOT=" + root,
				"PATH=" + root + ":" + os.Getenv("PATH"),
				"FAIL=" + tc.failure,
			}, `
source "$SCRIPT"
remote_exec() {
  if [[ "$2" == sudo\ bash\ -s* ]]; then
    local script
    script="$(</dev/stdin)"
    script="${script//\/etc\/aks-flex-node\/daemon-state.json/$ROOT\/state.json}"
    script="${script//\/etc\/aks-flex-node\/config.json/$ROOT\/agent.json}"
    script="${script//\/etc\/aks-flex-node\/daemon-credentials\/daemon-controller-current.pem/$ROOT\/daemon.pem}"
    bash -s -- nspawn <<<"$script"
  else
    bash -c "$2"
  fi
}
snapshot="$(_isolation_snapshot vm nspawn)"
[[ "$snapshot" == 'nspawn|kube2|1.35.0|settings|/var/lib/machines/kube2' ]]
config="$(_isolation_config_digests vm nspawn)"
credentials="$(_isolation_credentials vm nspawn kube2)"
[[ "$config" == *'|'* && -n "$credentials" ]]
for file in agent.json daemon.pem config; do
  cp "$ROOT/$file" "$ROOT/original"
  echo changed >> "$ROOT/$file"
  [[ "$(_isolation_config_digests vm nspawn)" != "$config" ]]
  mv "$ROOT/original" "$ROOT/$file"
done
`, tc.success)
			if strings.Contains(output, "fixture-secret") {
				t.Fatalf("fingerprint leaked contents: %s", output)
			}
			if tc.success {
				reads, err := os.ReadFile(filepath.Join(root, "reads"))
				if err != nil {
					t.Fatal(err)
				}
				for _, count := range strings.Fields(string(reads)) {
					if count != "0" {
						t.Fatalf("guest command consumed stdin: %q", reads)
					}
				}
			}
		})
	}
}
