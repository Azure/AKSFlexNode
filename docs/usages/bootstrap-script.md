# Generated Bootstrap Script

`scripts/bootstrap.sh` is a minimal first-boot entry point for joining a prepared
Linux host to an AKS FlexNodes pool. A publisher embeds cluster-specific join
configuration into the script, places the generated script at a protected URL,
and gives that URL to the machine provisioner.

The script then:

1. applies environment and CLI overrides to the embedded config;
2. defaults the node name from the host name;
3. selects managed identity or service-principal runtime authentication;
4. downloads and installs `aks-flex-node`;
5. writes `/etc/aks-flex-node/config.json` as `0600 root:root`;
6. runs `aks-flex-node preflight`;
7. runs `aks-flex-node start` only when preflight succeeds.

For architecture and security rationale, see
[Generated bootstrap script design](../design/storage-backed-bootstrap.md).

## When to use it

Use the generated script when:

- machines are created from a reusable VHD or marketplace image;
- the cluster bootstrap data is known before first boot;
- the host should not install or depend on Azure CLI;
- a provisioning system can securely deliver a generated script URL;
- runtime differences are limited to authentication, artifact selection, and
  small JSON overrides.

Use the normal manual flow instead when an operator already has a complete
config file and wants to run preflight and start separately.

## Prerequisites

The script does not install packages. The host image must already contain:

- Bash
- curl
- tar
- jq
- standard Linux core utilities
- `sha256sum` when an agent digest is supplied
- normal Flex Node prerequisites such as `systemd-container`, `nftables`, and
  `util-linux`

The host must also have:

- network connectivity to the AKS API server;
- connectivity to the agent artifact URL and bootstrap registries;
- a unique, Kubernetes-compatible host name;
- any managed identity or service-principal permissions required for ARM
  Machine registration;
- working time synchronization.

Run the script as root. It fails immediately when a direct command dependency is
missing.

## Generate the cluster-specific script

The repository script is a template. Near the top it contains one marker:

```text
__AKS_FLEX_NODE_BASE_CONFIG_JSON__
```

The publisher replaces that marker with a partial config generated for the
cluster and pool. The config should normally come from fresh pool bootstrap data
and include:

- cluster resource ID and location;
- target agent pool;
- bootstrap token;
- API server FQDN and CA;
- component versions;
- DNS/CNI settings;
- machine-client settings.

It should normally omit:

- `agent.nodeName`, because the script defaults it from the host name;
- `node.kubelet.nodeIP`, because the node runtime discovers the host address.

Example publisher code:

```python
from pathlib import Path

marker = "__AKS_FLEX_NODE_BASE_CONFIG_JSON__"
template = Path("scripts/bootstrap.sh").read_text()
config = Path("start-config.json").read_text().rstrip()

if template.count(marker) != 1:
    raise RuntimeError("expected exactly one embedded config marker")

output = template.replace(marker, config)
Path("generated-bootstrap.sh").write_text(output)
Path("generated-bootstrap.sh").chmod(0o700)
```

Validate the generated result without displaying its contents:

```bash
bash -n generated-bootstrap.sh
```

The generated script contains bootstrap credentials. Do not commit it, print
it, attach it to tickets, or retain it beyond the credential lifetime.

For development tests only, `AKS_FLEX_NODE_BASE_CONFIG_FILE` can supply the base
config without replacing the marker. Production callers should use an embedded
config.

## Download the generated script

The caller owns the initial script download. `--auth` does not authenticate this
step because the script is not running yet.

For an already readable URL:

```bash
install -d -m 0700 /run/aks-flex-node-bootstrap
curl -fsSLo /run/aks-flex-node-bootstrap/bootstrap.sh "$BOOTSTRAP_SCRIPT_URL"
chmod 0700 /run/aks-flex-node-bootstrap/bootstrap.sh
bash -n /run/aks-flex-node-bootstrap/bootstrap.sh
```

For a private Blob, the caller can use a signed URL or acquire a Storage token
with a managed identity before downloading. Do not print the URL or token.

Prefer saving and validating the script over piping directly to Bash:

```text
Recommended:      curl -o bootstrap.sh ... && bash -n bootstrap.sh && sudo bash bootstrap.sh
Not recommended:  curl ... | sudo bash
```

If the publisher provides a script checksum or signature, verify it before root
execution.

## Input precedence

The script resolves values in this order:

```text
CLI flags > environment variables > embedded config/script defaults
```

Dedicated authentication flags are applied after generic JSON overrides. This
prevents a generic override from accidentally leaving both MSI and SP runtime
authentication configured.

## Command reference

```text
--auth MODE
```

Select `msi` or `service-principal`. When omitted, the authentication already in
the embedded base config is preserved.

```text
--msi-client-id ID
```

Select a user-assigned identity. Omit it for a system-assigned identity or a
host where the default managed identity is unambiguous.

```text
--sp-tenant-id ID
--sp-client-id ID
--sp-client-secret-file PATH
```

Configure service-principal runtime authentication. Tenant defaults to
`azure.tenantId` in the base config. The secret file must not be accessible by
group or other users.

There is intentionally no client-secret CLI flag.

```text
--agent-url URL
```

Download an exact tar.gz URL. It may be HTTPS, `file://`, a mirror, or a URL
with an embedded SAS. The URL must already be readable by curl.

Supported URL placeholders:

```text
{{OS}}
{{ARCH}}
{{VERSION}}
{{ARCHIVE_NAME}}
```

```text
--agent-version VERSION
```

Use the default GitHub release URL:

```text
https://github.com/Azure/AKSFlexNode/releases/download/<version>/aks-flex-node-linux-<arch>.tar.gz
```

`--agent-url` takes precedence when both URL and version are supplied.

```text
--agent-sha256 SHA256
```

Verify the downloaded archive before extraction. Supplying a digest is strongly
recommended.

```text
--config-overrides JSON
```

Deep-merge a JSON object into the base config. The flag is repeatable and later
objects win. Do not put credentials in this flag because command arguments are
process-visible.

```text
--install-dir PATH
--config-path PATH
```

Override `/usr/local/bin` and `/etc/aks-flex-node/config.json`. These are mainly
useful for image integration tests; the installed systemd service expects the
normal production paths.

## Environment reference

| Variable | Purpose |
|---|---|
| `AKS_FLEX_NODE_AUTH` | `msi` or `service-principal` |
| `AKS_FLEX_NODE_MSI_CLIENT_ID` | User-assigned MSI client ID |
| `AKS_FLEX_NODE_SP_TENANT_ID` | Optional SP tenant override |
| `AKS_FLEX_NODE_SP_CLIENT_ID` | SP application/client ID |
| `AKS_FLEX_NODE_SP_CLIENT_SECRET` | Direct SP credential; prefer a file |
| `AKS_FLEX_NODE_SP_CLIENT_SECRET_FILE` | Protected SP credential file |
| `AKS_FLEX_NODE_AGENT_URL` | Exact agent archive URL |
| `AKS_FLEX_NODE_AGENT_VERSION` | Version for the default release URL |
| `AKS_FLEX_NODE_AGENT_SHA256` | Expected archive SHA-256 |
| `AKS_FLEX_NODE_CONFIG_OVERRIDES` | One JSON object merged before CLI overrides |
| `AKS_FLEX_NODE_INSTALL_DIR` | Binary directory override |
| `AKS_FLEX_NODE_CONFIG_PATH` | Config path override |

The secret file takes precedence over `AKS_FLEX_NODE_SP_CLIENT_SECRET`.
Bootstrap environment values, signed URLs, and direct secrets are cleared before
preflight and start child processes run.

When using sudo, pass an explicit allowlist:

```bash
sudo --preserve-env=AKS_FLEX_NODE_AUTH,AKS_FLEX_NODE_AGENT_URL,AKS_FLEX_NODE_AGENT_SHA256 \
  bash /run/aks-flex-node-bootstrap/bootstrap.sh
```

Alternatively, use `sudo env NAME=value ... bash bootstrap.sh`.

## Managed identity examples

### System-assigned identity

```bash
sudo bash bootstrap.sh \
  --auth msi \
  --agent-version v0.1.5
```

The script removes `azure.servicePrincipal`, disables Arc authentication, and
writes an empty `azure.managedIdentity` object. A bootstrap token can remain for
Kubernetes TLS bootstrap.

### User-assigned identity

```bash
sudo bash bootstrap.sh \
  --auth msi \
  --msi-client-id "$MANAGED_IDENTITY_CLIENT_ID" \
  --agent-url "$AGENT_URL" \
  --agent-sha256 "$AGENT_SHA256"
```

Ensure the same identity is assigned to the VM and has the required AKS role
before first boot runs. Allow time for role assignments to propagate.

## Service-principal examples

Create a protected credential file without printing it:

```bash
sudo install -d -m 0700 /run/credentials
sudo install -m 0600 /dev/null /run/credentials/aks-flex-node-sp
printf '%s' "$CLIENT_SECRET" | sudo tee /run/credentials/aks-flex-node-sp >/dev/null
```

Run bootstrap:

```bash
sudo bash bootstrap.sh \
  --auth service-principal \
  --sp-client-id "$CLIENT_ID" \
  --sp-client-secret-file /run/credentials/aks-flex-node-sp \
  --agent-url "$AGENT_URL" \
  --agent-sha256 "$AGENT_SHA256"
```

The script uses jq to safely encode quotes, backslashes, and other special
characters from the secret file. It removes managed identity and disables Arc
authentication.

Remove the transient secret file according to the credential-delivery system's
lifecycle after bootstrap succeeds.

## Config override examples

### Environment override for cloud-init

```bash
export AKS_FLEX_NODE_CONFIG_OVERRIDES='{
  "node": {
    "labels": {
      "aks-flex-node.azure.com/bootstrap-scenario": "cloud-init"
    }
  }
}'
```

### CLI overrides

```bash
sudo bash bootstrap.sh \
  --agent-url "$AGENT_URL" \
  --config-overrides '{"node":{"labels":{"node-type":"edge"}}}' \
  --config-overrides '{"node":{"maxPods":200}}'
```

The merge order is base, environment object, then CLI objects from left to
right.

## Offline or mirrored artifact

A local artifact still goes through curl, checksum validation, archive path
validation, and atomic installation:

```bash
sudo bash bootstrap.sh \
  --agent-url file:///opt/aks-flex-node/aks-flex-node-linux-amd64.tar.gz \
  --agent-sha256 "$AGENT_SHA256"
```

The archive must contain either:

```text
aks-flex-node-linux-amd64
aks-flex-node-linux-arm64
aks-flex-node
```

for the detected host architecture.

## Cloud-init example

Cloud-init must first obtain the generated script. The example below assumes the
script is already available at the local path shown:

```yaml
#cloud-config
packages:
  - curl
  - jq
  - nftables
  - systemd-container
  - tar
  - util-linux
runcmd:
  - - env
    - AKS_FLEX_NODE_AUTH=msi
    - AKS_FLEX_NODE_AGENT_URL=https://example/aks-flex-node-linux-amd64.tar.gz
    - AKS_FLEX_NODE_AGENT_SHA256=<sha256>
    - AKS_FLEX_NODE_CONFIG_OVERRIDES={"node":{"labels":{"bootstrap":"cloud-init"}}}
    - bash
    - /run/aks-flex-node-bootstrap/bootstrap.sh
```

For a private script URL, use caller-owned code to acquire a Storage token and
download it before the `runcmd` invocation. Runtime `AKS_FLEX_NODE_AUTH=msi`
does not authenticate the earlier download.

Cloud-init or a systemd oneshot wrapper should create a completion marker only
after the script exits successfully.

## Expected output and postconditions

Successful output includes messages similar to:

```text
bootstrap: downloading AKS Flex Node agent (URL redacted)
bootstrap: installed agent at /usr/local/bin/aks-flex-node
bootstrap: rendered config at /etc/aks-flex-node/config.json
bootstrap: running preflight
[preflight] Running AKS Flex Node preflight checks
bootstrap: starting AKS Flex Node
AKS Flex Node agent service started successfully.
```

After success, verify:

```bash
stat -c '%a %U:%G %n' /etc/aks-flex-node/config.json
systemctl is-active aks-flex-node-agent
systemctl is-enabled aks-flex-node-agent
```

Expected config mode:

```text
600 root:root /etc/aks-flex-node/config.json
```

From an authenticated workstation:

```bash
kubectl get node "$NODE_NAME" -o wide
```

Also verify the ARM Machine reached `Succeeded`, network site assignment
converged, and the node received a pod CIDR.

## Completion and cleanup

The generated script contains bootstrap credentials. Remove the downloaded copy
after success and write a caller-owned completion marker:

```bash
if bash /run/aks-flex-node-bootstrap/bootstrap.sh; then
  rm -f /run/aks-flex-node-bootstrap/bootstrap.sh
  install -d -m 0755 /var/lib/aks-flex-node
  touch /var/lib/aks-flex-node/first-boot-complete
fi
```

Do not bake that marker into a reusable image.

The full bootstrap script is intended to run once. A systemd wrapper can use:

```ini
ConditionPathExists=!/var/lib/aks-flex-node/first-boot-complete
```

to avoid rerunning after a successful reboot.

## Failure handling

The script exits nonzero on dependency, JSON, download, checksum, archive,
preflight, or start failures.

- Dependency/config/download/checksum failures happen before host bootstrap and
  can be retried after fixing the input.
- Preflight failures leave the protected rendered config available for
  diagnosis and do not call start.
- Start failures may leave partial host or ARM Machine state; inspect agent logs
  before retrying.
- Do not rerun the full script after success; existing-deployment preflight is
  expected to reject a second first-boot attempt.
- If the embedded bootstrap token expires, publish and download a new generated
  script.

Useful diagnostics:

```bash
journalctl -u aks-flex-node-agent --no-pager -n 200
sudo tail -n 200 /var/log/aks-flex-node/aks-flex-node.log
machinectl list
sudo systemctl -M kube1 status kubelet containerd node-problem-detector
```

A pending daemon CSR may require approval when the cluster does not run the
Flex Node CSR controller.

## Security checklist

- Protect the generated script like a credential.
- Use a short-lived bootstrap token and script URL.
- Prefer read-only, short-lived SAS URLs for private artifacts.
- Supply and verify agent SHA-256.
- Never put SP secrets in CLI arguments or generic JSON overrides.
- Keep SP secret files mode `0600` or stricter.
- Do not print the generated script, config, signed URLs, or secrets.
- Remove the downloaded script and transient secret files after success.
- Grant MSI/SP identities only the roles required by the selected runtime mode.
