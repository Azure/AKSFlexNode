# `aks-flex-node boot`

`aks-flex-node boot` is the first-boot orchestration command. It prepares the
active agent binary, optionally fetches fresh join data from AKS RP, renders and
validates the node config, runs preflight, and starts the node.

The existing command remains unchanged:

```console
aks-flex-node bootstrap --config /etc/aks-flex-node/config.json
```

`bootstrap` is still an alias for `start`. Use `boot` for end-to-end first-boot
orchestration.

## Example: service-principal certificate

```console
aks-flex-node boot \
  --fetch-bootstrap-data \
  --cluster-resource-id "$AKS_CLUSTER_RESOURCE_ID" \
  --agent-pool-name aksflexnodes \
  --auth service-principal \
  --sp-tenant-id "$TENANT_ID" \
  --sp-client-id "$CLIENT_ID" \
  --sp-client-certificate-file /run/credentials/aks-flex-node-certificate \
  --agent-version v0.1.6.alpha-2 \
  --agent-sha256 "$AGENT_SHA256" \
  --bootstrap-oci-image "$ROOTFS_SOURCE" \
  --bootstrap-offline-artifacts-source "$OFFLINE_ARTIFACT_TEMPLATE"
```

The certificate file can be a combined PEM certificate chain and RSA private
key with any filename, or an unencrypted PKCS#12 file with a `.pfx` suffix. It
must be an absolute path to a protected regular file.

The command uses Azure Identity for certificate authentication and sends the
certificate chain in `x5c`, supporting Subject Name/Issuer configurations as
well as directly registered certificates.

## Example: managed identity

```console
aks-flex-node boot \
  --fetch-bootstrap-data \
  --cluster-resource-id "$AKS_CLUSTER_RESOURCE_ID" \
  --agent-pool-name aksflexnodes \
  --auth msi \
  --agent-url 'file:///opt/aks-flex-node/aks-flex-node-linux-amd64.tar.gz' \
  --agent-sha256 "$AGENT_SHA256" \
  --bootstrap-oci-image "$ROOTFS_SOURCE" \
  --bootstrap-offline-artifacts-source "$OFFLINE_ARTIFACT_TEMPLATE"
```

Use `--msi-client-id` for a user-assigned identity.

## Agent source

The active agent can come from:

- `--agent-url` with HTTP, HTTPS, `file://`, or a local path;
- `--agent-version`, which resolves the normal GitHub release URL; or
- the currently running binary when neither is supplied.

URL templates support:

```text
{{OS}}
{{ARCH}}
{{VERSION}}
{{ARCHIVE_NAME}}
```

The archive is optionally verified with `--agent-sha256`, installed atomically
at `<install-dir>/aks-flex-node`, and re-executed before bootstrap continues.
This allows a baseline binary baked into a VHD to hand off to a pinned binary
from an offline filesystem or dedicated mirror.

## Config processing order

```text
optional base --config
→ cluster/pool/ARM endpoint overrides
→ optional listBootstrapData response
→ generic JSON config overrides
→ reapply cluster/pool/ARM endpoint overrides
→ rootfs/offline artifact source overrides
→ offline manifest version normalization
→ host node-name default
→ MSI/SP auth override
→ typed config validation
→ atomic mode-0600 config write
→ preflight
→ start
```

When offline artifacts are configured, their manifest is authoritative for
containerd, runc, and CNI versions. Kubernetes version remains explicit because
it selects the versioned bundle.

## Environment equivalents

CLI values override these environment variables:

```text
AKS_FLEX_NODE_AUTH
AKS_FLEX_NODE_MSI_CLIENT_ID
AKS_FLEX_NODE_SP_TENANT_ID
AKS_FLEX_NODE_SP_CLIENT_ID
AKS_FLEX_NODE_SP_CLIENT_SECRET
AKS_FLEX_NODE_SP_CLIENT_SECRET_FILE
AKS_FLEX_NODE_SP_CLIENT_CERTIFICATE_FILE
AKS_FLEX_NODE_FETCH_BOOTSTRAP_DATA
AKS_FLEX_NODE_BOOTSTRAP_DATA_API_VERSION
AKS_FLEX_NODE_CLUSTER_RESOURCE_ID
AKS_FLEX_NODE_AGENT_POOL_NAME
AKS_FLEX_NODE_RESOURCE_MANAGER_ENDPOINT
AKS_FLEX_NODE_BOOTSTRAP_OCI_IMAGE
AKS_FLEX_NODE_BOOTSTRAP_OFFLINE_ARTIFACTS_SOURCE
AKS_FLEX_NODE_CONFIG_OVERRIDES
AKS_FLEX_NODE_BASE_CONFIG_FILE
AKS_FLEX_NODE_CONFIG_PATH
AKS_FLEX_NODE_AGENT_URL
AKS_FLEX_NODE_AGENT_VERSION
AKS_FLEX_NODE_AGENT_SHA256
AKS_FLEX_NODE_INSTALL_DIR
AKS_FLEX_NODE_AUTHORITY_HOST
```

Prefer file-based credentials. Signed URLs supplied through environment
variables are removed before preflight and start subprocesses.

## Offline first boot

A fully disconnected invocation can use local inputs:

```console
aks-flex-node boot \
  --config /opt/aks-flex-node/base-config.json \
  --auth service-principal \
  --sp-tenant-id "$TENANT_ID" \
  --sp-client-id "$CLIENT_ID" \
  --sp-client-certificate-file /run/credentials/aks-flex-node-certificate \
  --agent-url file:///opt/aks-flex-node/aks-flex-node-linux-amd64.tar.gz \
  --agent-sha256 "$AGENT_SHA256" \
  --bootstrap-oci-image oci-layout:///opt/aks-flex-node/rootfs:v20260619 \
  --bootstrap-offline-artifacts-source 'file:///opt/aks-flex-node/artifacts/{{ .KubernetesVersion }}'
```

Omit `--fetch-bootstrap-data` when the base config already contains valid
cluster-issued bootstrap data.

## Security and lifecycle

- Config and binary writes are atomic.
- Final config is mode `0600`.
- Download and archive sizes are bounded.
- HTTPS downgrade redirects are rejected.
- Certificate and secret files are validated by the typed config loader.
- SP certificate auth uses Azure SDK assertions instead of shell-built JWTs.
- The command fails before `start` when config validation or preflight fails.
- This is a first-boot workflow; do not repeatedly run it after successful node
  enrollment.
