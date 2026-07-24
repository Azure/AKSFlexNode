#!/bin/bash
# AKS Flex Node first-boot script.
#
# Requirements: bash, curl, tar, and jq. sha256sum is required only when an
# artifact checksum is configured. This script does not install dependencies.
#
# The publisher must replace the marker in write_embedded_base_config with a
# cluster/pool-specific partial config before distributing this script.
# Usage guide: docs/usages/bootstrap-script.md

set -euo pipefail
umask 077

readonly DEFAULT_REPOSITORY="Azure/AKSFlexNode"
readonly DEFAULT_INSTALL_DIR="/usr/local/bin"
readonly DEFAULT_CONFIG_PATH="/etc/aks-flex-node/config.json"
readonly DEFAULT_BOOTSTRAP_DATA_API_VERSION="2026-05-02-preview"
readonly DEFAULT_AUTHORITY_HOST="https://login.microsoftonline.com"
readonly DEFAULT_IMDS_ENDPOINT="http://169.254.169.254/metadata/identity/oauth2/token"

# Publisher-managed cluster/pool config. Keep this block near the top so the
# script generation path only needs to replace the single marker below.
write_embedded_base_config() {
    cat <<'AKS_FLEX_NODE_EMBEDDED_CONFIG'
__AKS_FLEX_NODE_BASE_CONFIG_JSON__
AKS_FLEX_NODE_EMBEDDED_CONFIG
}

AUTH_MODE="${AKS_FLEX_NODE_AUTH:-}"
MSI_CLIENT_ID="${AKS_FLEX_NODE_MSI_CLIENT_ID:-}"
SP_TENANT_ID="${AKS_FLEX_NODE_SP_TENANT_ID:-}"
SP_CLIENT_ID="${AKS_FLEX_NODE_SP_CLIENT_ID:-}"
SP_CLIENT_SECRET="${AKS_FLEX_NODE_SP_CLIENT_SECRET:-}"
SP_CLIENT_SECRET_FILE="${AKS_FLEX_NODE_SP_CLIENT_SECRET_FILE:-}"
AGENT_URL="${AKS_FLEX_NODE_AGENT_URL:-}"
AGENT_VERSION="${AKS_FLEX_NODE_AGENT_VERSION:-}"
AGENT_SHA256="${AKS_FLEX_NODE_AGENT_SHA256:-}"
INSTALL_DIR="${AKS_FLEX_NODE_INSTALL_DIR:-$DEFAULT_INSTALL_DIR}"
CONFIG_PATH="${AKS_FLEX_NODE_CONFIG_PATH:-$DEFAULT_CONFIG_PATH}"
ENV_CONFIG_OVERRIDES="${AKS_FLEX_NODE_CONFIG_OVERRIDES:-}"
BASE_CONFIG_FILE="${AKS_FLEX_NODE_BASE_CONFIG_FILE:-}"
FETCH_BOOTSTRAP_DATA="${AKS_FLEX_NODE_FETCH_BOOTSTRAP_DATA:-false}"
BOOTSTRAP_DATA_API_VERSION="${AKS_FLEX_NODE_BOOTSTRAP_DATA_API_VERSION:-$DEFAULT_BOOTSTRAP_DATA_API_VERSION}"
AUTHORITY_HOST="${AKS_FLEX_NODE_AUTHORITY_HOST:-$DEFAULT_AUTHORITY_HOST}"
IMDS_ENDPOINT="${AKS_FLEX_NODE_IMDS_ENDPOINT:-$DEFAULT_IMDS_ENDPOINT}"
CONFIG_OVERRIDES=()
TEMP_DIR=""

log() {
    printf 'bootstrap: %s\n' "$*" >&2
}

fatal() {
    printf 'bootstrap: error: %s\n' "$*" >&2
    exit 1
}

usage() {
    cat <<'EOF'
Usage: bootstrap.sh [options]

Render the embedded AKS Flex Node config, install the agent, run preflight, and
start the node. CLI flags override environment variables, which override the
embedded base config and script defaults.

Options:
  --auth MODE                    msi or service-principal
  --msi-client-id ID             Optional user-assigned managed identity client ID
  --sp-tenant-id ID              Optional SP tenant; defaults to azure.tenantId
  --sp-client-id ID              Service-principal client ID
  --sp-client-secret-file PATH   Protected file containing the SP credential
  --agent-url URL                Exact agent tar.gz URL; may contain a SAS
  --agent-version VERSION        Version used with the default GitHub release URL
  --agent-sha256 SHA256          Expected SHA-256 of the downloaded tar.gz
  --fetch-bootstrap-data         Fetch and merge fresh AKS RP bootstrap data
  --bootstrap-data-api-version V API version for listBootstrapData
  --config-overrides JSON        JSON object deep-merged into the base config;
                                 repeatable and not suitable for secrets
  --install-dir PATH             Binary destination directory
  --config-path PATH             Rendered config destination
  -h, --help                     Show this help

Environment overrides:
  AKS_FLEX_NODE_AUTH
  AKS_FLEX_NODE_MSI_CLIENT_ID
  AKS_FLEX_NODE_SP_TENANT_ID
  AKS_FLEX_NODE_SP_CLIENT_ID
  AKS_FLEX_NODE_SP_CLIENT_SECRET
  AKS_FLEX_NODE_SP_CLIENT_SECRET_FILE
  AKS_FLEX_NODE_AGENT_URL
  AKS_FLEX_NODE_AGENT_VERSION
  AKS_FLEX_NODE_AGENT_SHA256
  AKS_FLEX_NODE_FETCH_BOOTSTRAP_DATA
  AKS_FLEX_NODE_BOOTSTRAP_DATA_API_VERSION
  AKS_FLEX_NODE_AUTHORITY_HOST
  AKS_FLEX_NODE_CONFIG_OVERRIDES
  AKS_FLEX_NODE_INSTALL_DIR
  AKS_FLEX_NODE_CONFIG_PATH

The client-secret file takes precedence over AKS_FLEX_NODE_SP_CLIENT_SECRET.
When invoking through sudo, explicitly preserve the required environment
variables. The script requires preinstalled bash, curl, tar, and jq.
EOF
}

require_value() {
    local option="$1"
    local value="${2:-}"
    [[ -n "$value" ]] || fatal "$option requires a value"
}

parse_args() {
    while (($# > 0)); do
        case "$1" in
            --auth|--msi-client-id|--sp-tenant-id|--sp-client-id|--sp-client-secret-file|--agent-url|--agent-version|--agent-sha256|--bootstrap-data-api-version|--config-overrides|--install-dir|--config-path)
                require_value "$1" "${2:-}"
                case "$1" in
                    --auth) AUTH_MODE="$2" ;;
                    --msi-client-id) MSI_CLIENT_ID="$2" ;;
                    --sp-tenant-id) SP_TENANT_ID="$2" ;;
                    --sp-client-id) SP_CLIENT_ID="$2" ;;
                    --sp-client-secret-file) SP_CLIENT_SECRET_FILE="$2" ;;
                    --agent-url) AGENT_URL="$2" ;;
                    --agent-version) AGENT_VERSION="$2" ;;
                    --agent-sha256) AGENT_SHA256="$2" ;;
                    --bootstrap-data-api-version) BOOTSTRAP_DATA_API_VERSION="$2" ;;
                    --config-overrides) CONFIG_OVERRIDES+=("$2") ;;
                    --install-dir) INSTALL_DIR="$2" ;;
                    --config-path) CONFIG_PATH="$2" ;;
                esac
                shift 2
                ;;
            --fetch-bootstrap-data)
                FETCH_BOOTSTRAP_DATA=true
                shift
                ;;
            -h|--help)
                usage
                exit 0
                ;;
            --)
                shift
                break
                ;;
            *) fatal "unknown option: $1" ;;
        esac
    done
    (($# == 0)) || fatal "unexpected positional arguments: $*"
}

cleanup() {
    if [[ -n "$TEMP_DIR" && -d "$TEMP_DIR" ]]; then
        rm -rf "$TEMP_DIR"
    fi
}

require_command() {
    command -v "$1" >/dev/null 2>&1 || fatal "required command not found: $1"
}

check_prerequisites() {
    local command
    for command in curl tar jq mktemp install find; do
        require_command "$command"
    done
    if [[ -n "$AGENT_SHA256" ]]; then
        require_command sha256sum
    fi
}

write_base_config() {
    local output="$1"
    if [[ -n "$BASE_CONFIG_FILE" ]]; then
        [[ -f "$BASE_CONFIG_FILE" ]] || fatal "base config file not found: $BASE_CONFIG_FILE"
        install -m 0600 "$BASE_CONFIG_FILE" "$output"
        return
    fi
    write_embedded_base_config > "$output"
}

merge_config_override() {
    local current="$1"
    local raw="$2"
    local label="$3"
    local override="$TEMP_DIR/config-override.json"
    local merged="$TEMP_DIR/config-merged.json"

    printf '%s' "$raw" > "$override"
    jq -e 'type == "object"' "$override" >/dev/null || fatal "$label must be a JSON object"
    jq -s '.[0] * .[1]' "$current" "$override" > "$merged"
    mv -f "$merged" "$current"
}

check_secret_file_permissions() {
    local path="$1"
    local permissions
    [[ -f "$path" ]] || fatal "service-principal client-secret file not found: $path"
    permissions=$(stat -c '%a' "$path")
    if (((8#$permissions & 077) != 0)); then
        fatal "service-principal client-secret file must not be accessible by group or other users"
    fi
}

is_true() {
    case "${1,,}" in
        true|1|yes|y) return 0 ;;
        *) return 1 ;;
    esac
}

resolve_fetch_auth_mode() {
    local current="$1"
    local mode="${AUTH_MODE,,}"
    if [[ -z "$mode" ]]; then
        if jq -e '.azure.managedIdentity != null' "$current" >/dev/null; then
            mode=msi
        elif jq -e '.azure.servicePrincipal != null' "$current" >/dev/null; then
            mode=service-principal
        else
            fatal "fetching bootstrap data requires MSI or service-principal authentication"
        fi
    fi
    case "$mode" in
        msi|managed-identity) printf 'msi' ;;
        sp|service-principal) printf 'service-principal' ;;
        *) fatal "fetching bootstrap data does not support auth mode: $mode" ;;
    esac
}

resolve_sp_secret_file() {
    local current="$1"
    local secret_file="$SP_CLIENT_SECRET_FILE"
    if [[ -n "$secret_file" ]]; then
        check_secret_file_permissions "$secret_file"
    elif [[ -n "$SP_CLIENT_SECRET" ]]; then
        secret_file="$TEMP_DIR/fetch-sp-client-secret"
        printf '%s' "$SP_CLIENT_SECRET" > "$secret_file"
        chmod 0600 "$secret_file"
    else
        secret_file="$TEMP_DIR/fetch-sp-client-secret"
        jq -er '.azure.servicePrincipal.clientSecret' "$current" > "$secret_file" || \
            fatal "service-principal bootstrap-data fetch requires a client secret"
        chmod 0600 "$secret_file"
    fi
    printf '%s' "$secret_file"
}

acquire_arm_token() {
    local current="$1"
    local arm_endpoint="$2"
    local output="$3"
    local mode token_response
    mode=$(resolve_fetch_auth_mode "$current")
    token_response="$TEMP_DIR/arm-token-response.json"

    case "$mode" in
        msi)
            local client_id="$MSI_CLIENT_ID"
            if [[ -z "$client_id" ]]; then
                client_id=$(jq -r '.azure.managedIdentity.clientId // ""' "$current")
            fi
            local curl_args=(
                -fsS --get
                -H 'Metadata: true'
                --data-urlencode 'api-version=2018-02-01'
                --data-urlencode "resource=${arm_endpoint%/}/"
                -o "$token_response"
            )
            if [[ -n "$client_id" ]]; then
                curl_args+=(--data-urlencode "client_id=$client_id")
            fi
            curl "${curl_args[@]}" "$IMDS_ENDPOINT" || fatal "failed to acquire managed identity ARM token"
            ;;
        service-principal)
            local tenant_id="$SP_TENANT_ID"
            local client_id="$SP_CLIENT_ID"
            local secret_file request_body token_url
            [[ -n "$tenant_id" ]] || tenant_id=$(jq -er '.azure.servicePrincipal.tenantId // .azure.tenantId' "$current")
            [[ -n "$client_id" ]] || client_id=$(jq -er '.azure.servicePrincipal.clientId' "$current")
            secret_file=$(resolve_sp_secret_file "$current")
            request_body="$TEMP_DIR/sp-token-request"
            jq -nr --arg clientID "$client_id" --arg scope "${arm_endpoint%/}/.default" --rawfile clientSecret "$secret_file" '
                ($clientSecret | rtrimstr("\n") | rtrimstr("\r")) as $secret |
                "client_id=\($clientID | @uri)&client_secret=\($secret | @uri)&scope=\($scope | @uri)&grant_type=client_credentials"
            ' > "$request_body"
            chmod 0600 "$request_body"
            token_url="${AUTHORITY_HOST%/}/${tenant_id}/oauth2/v2.0/token"
            curl -fsS -H 'Content-Type: application/x-www-form-urlencoded' \
                --data-binary "@$request_body" -o "$token_response" "$token_url" || \
                fatal "failed to acquire service-principal ARM token"
            ;;
    esac

    jq -erj '.access_token' "$token_response" > "$output" || fatal "ARM token response did not contain an access token"
    chmod 0600 "$output"
}

fetch_latest_bootstrap_data() {
    local current="$1"
    local arm_endpoint resource_id pool_name token_file header_file response merged url
    arm_endpoint=$(jq -r '.azure.resourceManagerEndpoint // "https://management.azure.com"' "$current")
    resource_id=$(jq -er '.azure.targetCluster.resourceId' "$current")
    pool_name=$(jq -er '.azure.targetAgentPoolName' "$current")
    [[ "$resource_id" == /subscriptions/*/providers/Microsoft.ContainerService/managedClusters/* ]] || \
        fatal "embedded config contains an invalid target cluster resource ID"
    [[ "$pool_name" =~ ^[A-Za-z0-9-]+$ ]] || fatal "embedded config contains an invalid target agent pool name"

    token_file="$TEMP_DIR/arm-access-token"
    header_file="$TEMP_DIR/arm-headers"
    response="$TEMP_DIR/bootstrap-data.json"
    merged="$TEMP_DIR/config-with-bootstrap-data.json"
    acquire_arm_token "$current" "$arm_endpoint" "$token_file"
    {
        printf 'Authorization: Bearer '
        cat "$token_file"
        printf '\nContent-Type: application/json\n'
    } > "$header_file"
    chmod 0600 "$header_file"

    url="${arm_endpoint%/}${resource_id}/agentPools/${pool_name}/listBootstrapData?api-version=${BOOTSTRAP_DATA_API_VERSION}"
    log "fetching fresh bootstrap data from AKS RP"
    curl -fsS -X POST -H "@$header_file" --data '' -o "$response" "$url" || \
        fatal "failed to fetch AKS bootstrap data"
    chmod 0600 "$response"
    jq -e 'type == "object" and (.azure.bootstrapToken.token | type == "string" and length > 0)' "$response" >/dev/null || \
        fatal "AKS bootstrap-data response did not contain a bootstrap token"
    jq -s '.[0] * .[1]' "$current" "$response" > "$merged"
    mv -f "$merged" "$current"
}

apply_auth_override() {
    local current="$1"
    local rendered="$TEMP_DIR/config-auth.json"
    local mode="${AUTH_MODE,,}"

    [[ -n "$mode" ]] || return 0
    case "$mode" in
        msi|managed-identity)
            jq --arg clientID "$MSI_CLIENT_ID" '
                .azure = (.azure // {}) |
                del(.azure.servicePrincipal) |
                .azure.arc = ((.azure.arc // {}) * {"enabled": false}) |
                .azure.managedIdentity = if $clientID == "" then {} else {"clientId": $clientID} end
            ' "$current" > "$rendered"
            ;;
        sp|service-principal)
            [[ -n "$SP_CLIENT_ID" ]] || fatal "service-principal auth requires --sp-client-id or AKS_FLEX_NODE_SP_CLIENT_ID"
            local secret_file="$SP_CLIENT_SECRET_FILE"
            if [[ -n "$secret_file" ]]; then
                check_secret_file_permissions "$secret_file"
            elif [[ -n "$SP_CLIENT_SECRET" ]]; then
                secret_file="$TEMP_DIR/sp-client-secret"
                printf '%s' "$SP_CLIENT_SECRET" > "$secret_file"
                chmod 0600 "$secret_file"
            else
                fatal "service-principal auth requires a protected secret file or AKS_FLEX_NODE_SP_CLIENT_SECRET"
            fi
            jq --arg clientID "$SP_CLIENT_ID" --arg tenantID "$SP_TENANT_ID" --rawfile clientSecret "$secret_file" '
                ($clientSecret | rtrimstr("\n") | rtrimstr("\r")) as $secret |
                .azure = (.azure // {}) |
                del(.azure.managedIdentity) |
                .azure.arc = ((.azure.arc // {}) * {"enabled": false}) |
                .azure.servicePrincipal = {
                    "tenantId": (if $tenantID == "" then .azure.tenantId else $tenantID end),
                    "clientId": $clientID,
                    "clientSecret": $secret
                }
            ' "$current" > "$rendered"
            ;;
        *) fatal "unsupported auth mode: $AUTH_MODE (expected msi or service-principal)" ;;
    esac
    mv -f "$rendered" "$current"
}

render_config() {
    local output="$1"
    local current="$TEMP_DIR/config-current.json"
    local rendered="$TEMP_DIR/config-rendered.json"
    local node_name

    write_base_config "$current"
    jq -e 'type == "object"' "$current" >/dev/null || fatal "embedded base config must be a JSON object"

    if is_true "$FETCH_BOOTSTRAP_DATA"; then
        fetch_latest_bootstrap_data "$current"
    fi

    if [[ -n "$ENV_CONFIG_OVERRIDES" ]]; then
        merge_config_override "$current" "$ENV_CONFIG_OVERRIDES" "AKS_FLEX_NODE_CONFIG_OVERRIDES"
    fi
    local override
    for override in "${CONFIG_OVERRIDES[@]}"; do
        merge_config_override "$current" "$override" "--config-overrides"
    done

    node_name=$(hostname | tr '[:upper:]' '[:lower:]')
    jq --arg nodeName "$node_name" '
        .agent = (.agent // {}) |
        if (.agent.nodeName // "") == "" then .agent.nodeName = $nodeName else . end
    ' "$current" > "$rendered"
    mv -f "$rendered" "$current"

    apply_auth_override "$current"
    jq -e . "$current" > "$output"
    chmod 0600 "$output"
}

detect_architecture() {
    case "$(uname -m)" in
        x86_64) printf 'amd64' ;;
        aarch64) printf 'arm64' ;;
        *) fatal "unsupported architecture: $(uname -m)" ;;
    esac
}

resolve_agent_url() {
    local arch="$1"
    local archive="aks-flex-node-linux-${arch}.tar.gz"
    local url="$AGENT_URL"
    if [[ -z "$url" ]]; then
        [[ -n "$AGENT_VERSION" ]] || fatal "set --agent-url/AKS_FLEX_NODE_AGENT_URL or --agent-version/AKS_FLEX_NODE_AGENT_VERSION"
        url="https://github.com/${DEFAULT_REPOSITORY}/releases/download/${AGENT_VERSION}/${archive}"
    fi
    url="${url//\{\{OS\}\}/linux}"
    url="${url//\{\{ARCH\}\}/$arch}"
    url="${url//\{\{VERSION\}\}/$AGENT_VERSION}"
    url="${url//\{\{ARCHIVE_NAME\}\}/$archive}"
    printf '%s' "$url"
}

validate_archive_paths() {
    local archive="$1"
    local listing="$TEMP_DIR/archive-listing"
    tar -tzf "$archive" > "$listing" || fatal "agent download is not a readable tar.gz archive"
    local entry
    while IFS= read -r entry; do
        [[ "$entry" != /* && "$entry" != ".." && "$entry" != ../* && "$entry" != */../* ]] || fatal "agent archive contains an unsafe path"
    done < "$listing"
}

download_and_install_agent() {
    local arch="$1"
    local url archive extract_dir expected candidate staged
    url=$(resolve_agent_url "$arch")
    archive="$TEMP_DIR/agent.tar.gz"
    extract_dir="$TEMP_DIR/agent"
    expected="aks-flex-node-linux-${arch}"

    log "downloading AKS Flex Node agent (URL redacted)"
    curl -fsSL --retry 3 --retry-delay 2 -o "$archive" "$url" || fatal "failed to download AKS Flex Node agent"

    if [[ -n "$AGENT_SHA256" ]]; then
        local actual
        actual=$(sha256sum "$archive" | awk '{print $1}')
        [[ "${actual,,}" == "${AGENT_SHA256,,}" ]] || fatal "agent archive SHA-256 mismatch"
    fi

    validate_archive_paths "$archive"
    mkdir -p "$extract_dir"
    tar -xzf "$archive" -C "$extract_dir"
    candidate=$(find "$extract_dir" -type f \( -name "$expected" -o -name aks-flex-node \) -print -quit)
    [[ -n "$candidate" ]] || fatal "agent binary not found in archive"

    install -d -o root -g root -m 0755 "$INSTALL_DIR"
    staged=$(mktemp "$INSTALL_DIR/.aks-flex-node.XXXXXX")
    install -o root -g root -m 0755 "$candidate" "$staged"
    mv -f "$staged" "$INSTALL_DIR/aks-flex-node"
    log "installed agent at $INSTALL_DIR/aks-flex-node"
}

clear_bootstrap_environment() {
    SP_CLIENT_SECRET=""
    AGENT_URL=""
    ENV_CONFIG_OVERRIDES=""
    unset \
        AKS_FLEX_NODE_AUTH \
        AKS_FLEX_NODE_MSI_CLIENT_ID \
        AKS_FLEX_NODE_SP_TENANT_ID \
        AKS_FLEX_NODE_SP_CLIENT_ID \
        AKS_FLEX_NODE_SP_CLIENT_SECRET \
        AKS_FLEX_NODE_SP_CLIENT_SECRET_FILE \
        AKS_FLEX_NODE_AGENT_URL \
        AKS_FLEX_NODE_AGENT_VERSION \
        AKS_FLEX_NODE_AGENT_SHA256 \
        AKS_FLEX_NODE_FETCH_BOOTSTRAP_DATA \
        AKS_FLEX_NODE_BOOTSTRAP_DATA_API_VERSION \
        AKS_FLEX_NODE_AUTHORITY_HOST \
        AKS_FLEX_NODE_IMDS_ENDPOINT \
        AKS_FLEX_NODE_CONFIG_OVERRIDES \
        AKS_FLEX_NODE_INSTALL_DIR \
        AKS_FLEX_NODE_CONFIG_PATH || true
}

install_config() {
    local rendered="$1"
    local config_dir staged
    config_dir=$(dirname "$CONFIG_PATH")
    install -d -o root -g root -m 0755 "$config_dir"
    staged=$(mktemp "$config_dir/.config.json.XXXXXX")
    install -o root -g root -m 0600 "$rendered" "$staged"
    mv -f "$staged" "$CONFIG_PATH"
    log "rendered config at $CONFIG_PATH"
}

main() {
    parse_args "$@"
    [[ $EUID -eq 0 ]] || fatal "run this script as root"
    check_prerequisites

    TEMP_DIR=$(mktemp -d)
    chmod 0700 "$TEMP_DIR"
    trap cleanup EXIT

    local arch rendered_config
    arch=$(detect_architecture)
    rendered_config="$TEMP_DIR/config.json"
    render_config "$rendered_config"
    download_and_install_agent "$arch"
    install_config "$rendered_config"
    clear_bootstrap_environment

    log "running preflight"
    "$INSTALL_DIR/aks-flex-node" preflight --config "$CONFIG_PATH" --output text
    log "starting AKS Flex Node"
    umask 022
    "$INSTALL_DIR/aks-flex-node" start --config "$CONFIG_PATH"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
    main "$@"
fi
