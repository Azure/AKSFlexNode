#!/usr/bin/env bash
#
# vm.sh - Manage QEMU-based Ubuntu VMs with cloud-init support
#
# Usage:
#   ./hack/qemu/vm.sh <command> [options]
#
# Commands:
#   start          Create and start a VM
#   stop           Stop a running VM
#   logs           Show VM serial console logs
#
# Start options:
#   -n, --name        VM name (default: flexnode-vm)
#   -m, --memory      Memory in MB (default: 2048)
#   -c, --cpus        Number of CPUs (default: 2)
#   -d, --disk-size   Disk size (default: 20G)
#   -p, --ssh-port    Host port forwarded to guest SSH (default: 2222)
#   -i, --image       Path to Ubuntu cloud image (downloaded if not present)
#   -u, --user-data   Path to cloud-init user-data file (default: hack/qemu/user-data.yaml)
#       --no-snapshot  Use the base image directly instead of creating a snapshot
#
# Stop options:
#   -n, --name   VM name (default: flexnode-vm)
#   -f, --force  Force kill (SIGKILL) instead of graceful shutdown (SIGTERM)
#       --clean  Also remove disk, seed ISO, and log files
#
# Logs options:
#   -n, --name   VM name (default: flexnode-vm)
#   -f, --follow  Follow log output (like tail -f)
#
# Examples:
#   ./hack/qemu/vm.sh start
#   ./hack/qemu/vm.sh start -n my-vm --memory 4096 --cpus 4
#   ./hack/qemu/vm.sh stop
#   ./hack/qemu/vm.sh stop --force --clean
#   ./hack/qemu/vm.sh logs
#   ./hack/qemu/vm.sh logs --follow
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

# -------------------------------------------------------------------
# Detect host architecture
# -------------------------------------------------------------------
HOST_ARCH="$(uname -m)"
case "${HOST_ARCH}" in
    x86_64)        GUEST_ARCH="amd64" ;;
    aarch64|arm64) GUEST_ARCH="arm64" ;;
    *)             echo "[ERROR] Unsupported host architecture: ${HOST_ARCH}" >&2; exit 1 ;;
esac

# -------------------------------------------------------------------
# Defaults
# -------------------------------------------------------------------
VM_NAME="flexnode-vm"
MEMORY="2048"
CPUS="2"
DISK_SIZE="20G"
SSH_PORT="2222"
USE_SNAPSHOT=true
FORCE=false
CLEAN=false

VM_DIR="${REPO_ROOT}/.vm"
IMAGE_BASE_URL="https://cloud-images.ubuntu.com/minimal/releases/noble/release"
IMAGE_URL="${IMAGE_BASE_URL}/ubuntu-24.04-minimal-cloudimg-${GUEST_ARCH}.img"
IMAGE_FILE=""
USER_DATA="${SCRIPT_DIR}/user-data.yaml"

# -------------------------------------------------------------------
# Helpers
# -------------------------------------------------------------------
info()  { echo "[INFO]  $*"; }
warn()  { echo "[WARN]  $*" >&2; }
error() { echo "[ERROR] $*" >&2; exit 1; }

usage() {
    cat <<'EOF'
Usage:
  ./hack/qemu/vm.sh <command> [options]

Commands:
  start          Create and start a VM
  stop           Stop a running VM
  logs           Show VM serial console logs

Start options:
  -n, --name        VM name (default: flexnode-vm)
  -m, --memory      Memory in MB (default: 2048)
  -c, --cpus        Number of CPUs (default: 2)
  -d, --disk-size   Disk size (default: 20G)
  -p, --ssh-port    Host port forwarded to guest SSH (default: 2222)
  -i, --image       Path to Ubuntu cloud image (downloaded if not present)
  -u, --user-data   Path to cloud-init user-data file (default: hack/qemu/user-data.yaml)
      --no-snapshot  Use the base image directly instead of creating a snapshot

Stop options:
  -n, --name   VM name (default: flexnode-vm)
  -f, --force  Force kill (SIGKILL) instead of graceful shutdown (SIGTERM)
      --clean  Also remove disk, seed ISO, and log files

Logs options:
  -n, --name    VM name (default: flexnode-vm)
  -f, --follow  Follow log output (like tail -f)

Examples:
  ./hack/qemu/vm.sh start
  ./hack/qemu/vm.sh start -n my-vm --memory 4096 --cpus 4
  ./hack/qemu/vm.sh stop
  ./hack/qemu/vm.sh stop --force --clean
  ./hack/qemu/vm.sh logs
  ./hack/qemu/vm.sh logs --follow
EOF
    exit 0
}

check_deps() {
    local qemu_bin
    if [[ "${GUEST_ARCH}" == "arm64" ]]; then
        qemu_bin="qemu-system-aarch64"
    else
        qemu_bin="qemu-system-x86_64"
    fi

    local missing=()
    for cmd in "${qemu_bin}" qemu-img; do
        if ! command -v "$cmd" &>/dev/null; then
            missing+=("$cmd")
        fi
    done

    # We need at least one ISO generation tool
    if ! command -v mkisofs &>/dev/null && ! command -v genisoimage &>/dev/null && ! command -v hdiutil &>/dev/null; then
        missing+=("mkisofs (or genisoimage or hdiutil)")
    fi

    if [[ ${#missing[@]} -gt 0 ]]; then
        echo ""
        echo "Missing required dependencies: ${missing[*]}"
        echo ""
        echo "Install on macOS:"
        echo "  brew install qemu cdrtools"
        echo ""
        echo "Install on Ubuntu/Debian:"
        echo "  sudo apt-get install qemu-system-x86 qemu-utils genisoimage"
        echo ""
        exit 1
    fi
}

# Build a cloud-init NoCloud seed ISO without requiring cloud-localds.
# Uses mkisofs, genisoimage, or hdiutil (macOS) — whichever is available.
create_seed_iso() {
    local iso_path="$1"
    local user_data="$2"
    local meta_data="$3"

    local staging
    staging="$(mktemp -d)"
    cp "${user_data}" "${staging}/user-data"
    cp "${meta_data}" "${staging}/meta-data"

    if command -v mkisofs &>/dev/null; then
        mkisofs -output "${iso_path}" -volid cidata -joliet -rock \
            "${staging}/user-data" "${staging}/meta-data"
    elif command -v genisoimage &>/dev/null; then
        genisoimage -output "${iso_path}" -volid cidata -joliet -rock \
            "${staging}/user-data" "${staging}/meta-data"
    elif command -v hdiutil &>/dev/null; then
        hdiutil makehybrid -o "${iso_path}" -joliet -iso \
            -default-volume-name cidata "${staging}"
    else
        rm -rf "${staging}"
        error "No ISO generation tool found"
    fi

    rm -rf "${staging}"
}

# ===================================================================
# Command: start
# ===================================================================
cmd_start() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            -n|--name)       VM_NAME="$2"; shift 2 ;;
            -m|--memory)     MEMORY="$2"; shift 2 ;;
            -c|--cpus)       CPUS="$2"; shift 2 ;;
            -d|--disk-size)  DISK_SIZE="$2"; shift 2 ;;
            -p|--ssh-port)   SSH_PORT="$2"; shift 2 ;;
            -i|--image)      IMAGE_FILE="$2"; shift 2 ;;
            -u|--user-data)  USER_DATA="$2"; shift 2 ;;
            --no-snapshot)   USE_SNAPSHOT=false; shift ;;
            -h|--help)       usage ;;
            *)               error "Unknown option: $1" ;;
        esac
    done

    check_deps
    mkdir -p "${VM_DIR}"

    # ---------------------------------------------------------------
    # Download Ubuntu cloud image if needed
    # ---------------------------------------------------------------
    if [[ -z "${IMAGE_FILE}" ]]; then
        IMAGE_FILE="${VM_DIR}/ubuntu-cloud.img"
    fi

    if [[ ! -f "${IMAGE_FILE}" ]]; then
        info "Downloading Ubuntu cloud image..."
        info "URL: ${IMAGE_URL}"
        curl -L -o "${IMAGE_FILE}" "${IMAGE_URL}"
        info "Download complete: ${IMAGE_FILE}"
    else
        info "Using existing image: ${IMAGE_FILE}"
    fi

    # ---------------------------------------------------------------
    # Create VM disk (snapshot backed by the cloud image)
    # ---------------------------------------------------------------
    VM_DISK="${VM_DIR}/${VM_NAME}.qcow2"

    if [[ "${USE_SNAPSHOT}" == true ]]; then
        info "Creating snapshot disk: ${VM_DISK} (backed by base image)"
        qemu-img create -f qcow2 -b "${IMAGE_FILE}" -F qcow2 "${VM_DISK}" "${DISK_SIZE}"
    else
        info "Copying base image to: ${VM_DISK}"
        cp "${IMAGE_FILE}" "${VM_DISK}"
        qemu-img resize "${VM_DISK}" "${DISK_SIZE}"
    fi

    # ---------------------------------------------------------------
    # Resolve local SSH public key
    # ---------------------------------------------------------------
    SSH_PUB_KEY=""
    for key_file in "${HOME}/.ssh/id_ed25519.pub" "${HOME}/.ssh/id_rsa.pub" "${HOME}/.ssh/id_ecdsa.pub"; do
        if [[ -f "${key_file}" ]]; then
            SSH_PUB_KEY="$(cat "${key_file}")"
            info "Using SSH public key: ${key_file}"
            break
        fi
    done

    if [[ -z "${SSH_PUB_KEY}" ]]; then
        warn "No SSH public key found in ~/.ssh/. The VM will not have key-based SSH access."
    fi

    # ---------------------------------------------------------------
    # Render user-data with SSH key into .vm/
    # ---------------------------------------------------------------
    RENDERED_USER_DATA="${VM_DIR}/user-data.yaml"

    if [[ ! -f "${USER_DATA}" ]]; then
        error "User-data template not found: ${USER_DATA}"
    fi

    if [[ -n "${SSH_PUB_KEY}" ]]; then
        sed "s|__SSH_PUBLIC_KEY__|${SSH_PUB_KEY}|g" "${USER_DATA}" > "${RENDERED_USER_DATA}"
    else
        # Remove the placeholder line entirely if no key is available
        sed '/__SSH_PUBLIC_KEY__/d' "${USER_DATA}" > "${RENDERED_USER_DATA}"
    fi
    info "Rendered user-data: ${RENDERED_USER_DATA}"

    # ---------------------------------------------------------------
    # Build cloud-init seed ISO
    # ---------------------------------------------------------------
    SEED_ISO="${VM_DIR}/${VM_NAME}-seed.iso"
    META_DATA="${VM_DIR}/meta-data"

    # Create minimal meta-data
    cat > "${META_DATA}" <<EOF
instance-id: ${VM_NAME}
local-hostname: ${VM_NAME}
EOF

    info "Creating cloud-init seed ISO: ${SEED_ISO}"
    create_seed_iso "${SEED_ISO}" "${RENDERED_USER_DATA}" "${META_DATA}"

    # ---------------------------------------------------------------
    # Determine QEMU binary, accelerator, and machine type
    # ---------------------------------------------------------------
    ACCEL=""
    MACHINE_ARGS=""
    QEMU_BIN=""

    if [[ "${GUEST_ARCH}" == "arm64" ]]; then
        QEMU_BIN="qemu-system-aarch64"
        MACHINE_ARGS="-machine virt -cpu host"

        # Locate UEFI firmware for aarch64
        UEFI_FW=""
        for fw_path in \
            /opt/homebrew/share/qemu/edk2-aarch64-code.fd \
            /usr/local/share/qemu/edk2-aarch64-code.fd \
            /usr/share/qemu-efi-aarch64/QEMU_EFI.fd \
            /usr/share/AAVMF/AAVMF_CODE.fd; do
            if [[ -f "${fw_path}" ]]; then
                UEFI_FW="${fw_path}"
                break
            fi
        done
        if [[ -z "${UEFI_FW}" ]]; then
            error "Could not find UEFI firmware for aarch64. Install qemu (brew install qemu) or edk2."
        fi
        MACHINE_ARGS="${MACHINE_ARGS} -bios ${UEFI_FW}"
    else
        QEMU_BIN="qemu-system-x86_64"
        MACHINE_ARGS="-cpu host"
    fi

    case "$(uname -s)" in
        Darwin)
            if sysctl -n kern.hv_support 2>/dev/null | grep -q 1; then
                ACCEL="-accel hvf"
            fi
            ;;
        Linux)
            if [[ -r /dev/kvm ]]; then
                ACCEL="-accel kvm"
            fi
            ;;
    esac

    # ---------------------------------------------------------------
    # Launch VM in background
    # ---------------------------------------------------------------
    QEMU_PID_FILE="${VM_DIR}/${VM_NAME}.pid"
    QEMU_LOG="${VM_DIR}/${VM_NAME}.log"

    info "============================================"
    info "  Launching VM: ${VM_NAME}"
    info "  Arch:         ${GUEST_ARCH} (${HOST_ARCH})"
    info "  Memory:       ${MEMORY} MB"
    info "  CPUs:         ${CPUS}"
    info "  Disk:         ${VM_DISK}"
    info "  SSH port:     ${SSH_PORT} -> 22"
    info "  Mount:        ${REPO_ROOT} -> /flex-node"
    info "  Log:          ${QEMU_LOG}"
    info "  PID file:     ${QEMU_PID_FILE}"
    info "============================================"

    # shellcheck disable=SC2086
    "${QEMU_BIN}" \
        ${MACHINE_ARGS} \
        ${ACCEL} \
        -m "${MEMORY}" \
        -smp "${CPUS}" \
        -drive file="${VM_DISK}",format=qcow2,if=virtio \
        -drive file="${SEED_ISO}",format=raw,if=virtio \
        -netdev user,id=net0,hostfwd=tcp::"${SSH_PORT}"-:22 \
        -device virtio-net-pci,netdev=net0 \
        -virtfs local,path="${REPO_ROOT}",mount_tag=flexnode,security_model=mapped-xattr,id=flexnode0 \
        -daemonize \
        -pidfile "${QEMU_PID_FILE}" \
        -serial file:"${QEMU_LOG}" \
        -display none

    QEMU_PID="$(cat "${QEMU_PID_FILE}")"
    info "VM started in background (PID: ${QEMU_PID})"

    # ---------------------------------------------------------------
    # Wait for SSH to become available
    # ---------------------------------------------------------------
    info "Waiting for SSH to become available on localhost:${SSH_PORT}..."

    MAX_ATTEMPTS=60
    ATTEMPT=0
    while [[ ${ATTEMPT} -lt ${MAX_ATTEMPTS} ]]; do
        ATTEMPT=$((ATTEMPT + 1))

        # Check that the QEMU process is still alive
        if ! kill -0 "${QEMU_PID}" 2>/dev/null; then
            echo ""
            error "QEMU process exited unexpectedly. Check log: ${QEMU_LOG}"
        fi

        if ssh -o BatchMode=yes -o ConnectTimeout=2 -o StrictHostKeyChecking=no \
               -o UserKnownHostsFile=/dev/null -p "${SSH_PORT}" ubuntu@localhost \
               "true" 2>/dev/null; then
            break
        fi

        printf "."
        sleep 3
    done
    echo ""

    if [[ ${ATTEMPT} -ge ${MAX_ATTEMPTS} ]]; then
        warn "SSH did not become available after ${MAX_ATTEMPTS} attempts."
        warn "The VM may still be booting. Check log: ${QEMU_LOG}"
        echo ""
        echo "You can try connecting manually:"
        echo ""
        echo "  ssh -o StrictHostKeyChecking=no -p ${SSH_PORT} ubuntu@localhost"
        echo ""
        echo "To stop the VM:"
        echo "  ./hack/qemu/vm.sh stop -n ${VM_NAME}"
        exit 1
    fi

    info "VM is ready!"
    echo ""
    echo "  ssh -o StrictHostKeyChecking=no -p ${SSH_PORT} ubuntu@localhost"
    echo ""
    echo "To stop the VM:"
    echo "  ./hack/qemu/vm.sh stop -n ${VM_NAME}"
    echo ""
}

# ===================================================================
# Command: stop
# ===================================================================
cmd_stop() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            -n|--name)   VM_NAME="$2"; shift 2 ;;
            -f|--force)  FORCE=true; shift ;;
            --clean)     CLEAN=true; shift ;;
            -h|--help)   usage ;;
            *)           error "Unknown option: $1" ;;
        esac
    done

    local pid_file="${VM_DIR}/${VM_NAME}.pid"

    if [[ ! -f "${pid_file}" ]]; then
        error "PID file not found: ${pid_file}. Is the VM running?"
    fi

    local pid
    pid="$(cat "${pid_file}")"

    if ! kill -0 "${pid}" 2>/dev/null; then
        warn "Process ${pid} is not running. Cleaning up stale PID file."
        rm -f "${pid_file}"
    else
        if [[ "${FORCE}" == true ]]; then
            info "Force killing VM '${VM_NAME}' (PID: ${pid})..."
            kill -9 "${pid}"
        else
            info "Stopping VM '${VM_NAME}' (PID: ${pid})..."
            kill "${pid}"

            # Wait for process to exit
            local timeout=15
            while kill -0 "${pid}" 2>/dev/null && [[ ${timeout} -gt 0 ]]; do
                sleep 1
                timeout=$((timeout - 1))
            done

            if kill -0 "${pid}" 2>/dev/null; then
                warn "VM did not stop gracefully, sending SIGKILL..."
                kill -9 "${pid}" 2>/dev/null || true
            fi
        fi

        rm -f "${pid_file}"
        info "VM '${VM_NAME}' stopped."
    fi

    if [[ "${CLEAN}" == true ]]; then
        info "Cleaning up VM artifacts..."
        rm -f "${VM_DIR}/${VM_NAME}.qcow2"
        rm -f "${VM_DIR}/${VM_NAME}-seed.iso"
        rm -f "${VM_DIR}/${VM_NAME}.log"
        rm -f "${VM_DIR}/user-data.yaml"
        rm -f "${VM_DIR}/meta-data"
        info "Cleanup complete."
    fi
}

# ===================================================================
# Command: logs
# ===================================================================
cmd_logs() {
    local follow=false

    while [[ $# -gt 0 ]]; do
        case "$1" in
            -n|--name)   VM_NAME="$2"; shift 2 ;;
            -f|--follow) follow=true; shift ;;
            -h|--help)   usage ;;
            *)           error "Unknown option: $1" ;;
        esac
    done

    local log_file="${VM_DIR}/${VM_NAME}.log"

    if [[ ! -f "${log_file}" ]]; then
        error "Log file not found: ${log_file}. Has the VM been started?"
    fi

    if [[ "${follow}" == true ]]; then
        info "Following logs for '${VM_NAME}' (Ctrl-C to stop)..."
        tail -f "${log_file}"
    else
        cat "${log_file}"
    fi
}

# ===================================================================
# Main: dispatch subcommand
# ===================================================================
if [[ $# -lt 1 ]]; then
    usage
fi

COMMAND="$1"
shift

case "${COMMAND}" in
    start) cmd_start "$@" ;;
    stop)  cmd_stop "$@" ;;
    logs)  cmd_logs "$@" ;;
    -h|--help) usage ;;
    *)     error "Unknown command: ${COMMAND}. Use 'start', 'stop', or 'logs'." ;;
esac
