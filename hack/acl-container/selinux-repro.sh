#!/bin/bash
# Reproduce, on Azure Container Linux, that a privileged containerd container
# cannot use the host's D-Bus under SELinux, and check everything else the AKS
# Flex Node agent does on the host from such a container. See SELINUX.md next
# to this file.
#
# Run as root on an ACL host that can pull IMAGE:
#
#   sudo bash selinux-repro.sh                 the environment, and the failure
#   sudo bash selinux-repro.sh --policy FILE   also the container under the binary
#                                              policy FILE, loaded for that probe;
#                                              repeat it to compare several
#   sudo bash selinux-repro.sh --workaround    also the container in kernel_t,
#                                              through a relabeled native snapshotter
#
# Options combine, and a summary ends the output. With MACHINE=NAME, an
# existing, running systemd-nspawn machine, each probe also drives that machine,
# and creates a second one from a copy of its root filesystem, as the agent
# creates machines, and drives that.
#
# Everything runs in its own containerd namespace, which is removed at the end.
# After each probe the host is put back: what the probe created is removed,
# and the masked unit, the module, the machine's enablement, and its running
# state are as before. --policy reloads the original policy after its probe;
# --workaround puts back the labels it changes.
set -euo pipefail

IMAGE=${IMAGE:-mcr.microsoft.com/azurelinux/busybox:1.36}
MACHINE=${MACHINE:-}
# A unit in /usr that is safe to mask for a moment, as the agent masks units.
MASK_UNIT=${MASK_UNIT:-systemd-pstore.service}
NS=acl-selinux-repro
REPRO=acl-selinux-repro
# Interface names are limited to 15 characters.
IFACE=aclrepro0
UNIT=$REPRO.service
NEW_MACHINE=$REPRO-m
NATIVE=/var/lib/containerd/io.containerd.snapshotter.v1.native

workaround=0
candidate_policies=()
while (( $# > 0 )); do
    case $1 in
        --workaround) workaround=1; shift ;;
        --policy) candidate_policies+=("${2:?--policy needs a file}"); shift 2 ;;
        *) echo "usage: $0 [--policy FILE]... [--workaround]" >&2; exit 2 ;;
    esac
done
[[ $EUID -eq 0 ]] || { echo "run as root" >&2; exit 1; }
for f in "${candidate_policies[@]}"; do
    [[ -f $f ]] || { echo "no such file: $f" >&2; exit 1; }
done
if [[ -n $MACHINE ]] && [[ $(machinectl show "$MACHINE" -p State --value 2>/dev/null) != running ]]; then
    echo "MACHINE=$MACHINE is not a running machine" >&2
    exit 1
fi

section() { printf '\n== %s\n' "$*"; }
type_of() { cut -d: -f3 <<<"$1"; }

# shellcheck source=/dev/null
policy_dir=/etc/selinux/$(. /etc/selinux/config && echo "$SELINUXTYPE")/policy
policy_files=("$policy_dir"/policy.*)
policy_file=${policy_files[-1]}
policy_backup=
native_labels=()
summary=()

# The host's state before any probe, which reset_host restores.
blockdev=$(find /sys/block -mindepth 1 -maxdepth 1 -printf '%f\n' | grep -vE '^(loop|ram|sr|zram|dm-|nbd)' | sort | head -1)
mask_unit_state=$(systemctl is-enabled "$MASK_UNIT" 2>/dev/null || true)
[[ -n $mask_unit_state && $mask_unit_state != not-found ]] || MASK_UNIT=
dummy_loaded=$([[ -d /sys/module/dummy ]] && echo yes || echo no)
machine_enabled=
if [[ -n $MACHINE ]]; then
    machine_enabled=$(systemctl is-enabled "systemd-nspawn@$MACHINE.service" 2>/dev/null || true)
fi

# Undo what a probe may have changed on the host. The probe's own commands can
# be denied halfway, so this runs as root on the host after every probe.
reset_host() {
    systemctl stop "$REPRO-timer.timer" "$REPRO-timer.service" 2>/dev/null || true
    rm -f "/etc/systemd/system/$UNIT" "/etc/systemd/system/multi-user.target.wants/$UNIT"
    if [[ -n $MASK_UNIT && $mask_unit_state != masked ]]; then
        systemctl unmask --quiet "$MASK_UNIT" || true
    fi
    systemctl daemon-reload
    systemctl reset-failed "$UNIT" 2>/dev/null || true
    ip link del "$IFACE" 2>/dev/null || true
    [[ $dummy_loaded == no ]] && modprobe -r dummy 2>/dev/null || true
    nft delete table inet acl_selinux_repro 2>/dev/null || true
    umount "/run/$REPRO-bpf" 2>/dev/null || true
    rmdir "/run/$REPRO-bpf" 2>/dev/null || true
    rm -rf "/opt/$REPRO" "/var/lib/machines/$REPRO-img"
    if [[ -n $(machinectl show "$NEW_MACHINE" -p State --value 2>/dev/null) ]]; then
        machinectl terminate "$NEW_MACHINE" || true
        for _ in $(seq 30); do
            [[ -z $(machinectl show "$NEW_MACHINE" -p State --value 2>/dev/null) ]] && break
            sleep 1
        done
    fi
    systemctl disable --quiet "systemd-nspawn@$NEW_MACHINE.service" 2>/dev/null || true
    systemctl reset-failed "systemd-nspawn@$NEW_MACHINE.service" 2>/dev/null || true
    rm -rf "/var/lib/machines/$NEW_MACHINE"
    rm -f "/etc/systemd/nspawn/$NEW_MACHINE.nspawn"
    if [[ -n $MACHINE ]]; then
        if [[ $machine_enabled == enabled ]]; then
            systemctl enable --quiet "systemd-nspawn@$MACHINE.service" || true
        else
            systemctl disable --quiet "systemd-nspawn@$MACHINE.service" || true
        fi
        if [[ $(machinectl show "$MACHINE" -p State --value 2>/dev/null) != running ]]; then
            machinectl start "$MACHINE" || true
        fi
    fi
}

restore_policy() {
    cp "$policy_backup" "$policy_file" && load_policy && rm -f "$policy_backup"
    policy_backup=
    echo "  restored the original policy ($(sha256sum <"$policy_file" | cut -c1-16))"
}

cleanup() {
    [[ -n $policy_backup ]] && restore_policy
    reset_host >/dev/null 2>&1 || true
    if (( ${#native_labels[@]} > 0 )); then
        chcon "${native_labels[0]}" "$NATIVE" || true
        chcon "${native_labels[1]}" "$NATIVE/snapshots" || true
    fi
    ctr -n "$NS" tasks kill -s SIGKILL probe >/dev/null 2>&1 || true
    ctr -n "$NS" containers rm probe >/dev/null 2>&1 || true
    ctr -n "$NS" images rm "$IMAGE" >/dev/null 2>&1 || true
    # containerd removes the image's content and snapshots in the background,
    # and a namespace can only be removed once empty.
    for _ in $(seq 30); do
        ctr namespaces rm "$NS" >/dev/null 2>&1 && break
        sleep 1
    done
}
trap cleanup EXIT

# The container's checks. Each runs a host binary under chroot /host, so it is
# the host's tool and libraries, but the container's SELinux context. It is
# expanded in the container, hence the single quotes.
# shellcheck disable=SC2016
PROBE='
echo "  container context: $(cat /proc/self/attr/current)"
run() {
    out=$(chroot /host /usr/bin/env "$@" 2>&1); rc=$?
    printf "  %-62s exit %s  %s\n" "$*" "$rc" "$(printf "%s" "$out" | head -1)"
    return $rc
}
label() { echo "  $2 label: $(chroot /host /usr/bin/stat -c %C "$1")"; }
state() { chroot /host /usr/bin/machinectl show "$1" -p State --value 2>/dev/null; }
# wait_running NAME: wait for the machine, then for the system bus in it, which
# comes up after machined reports the machine running.
wait_running() {
    i=0; while [ "$(state "$1")" != running ] && [ $i -lt 60 ]; do sleep 1; i=$((i + 1)); done
    i=0
    while ! chroot /host /usr/bin/systemd-run --machine="$1" --pipe --wait --quiet true >/dev/null 2>&1 &&
        [ $i -lt 60 ]; do
        sleep 1; i=$((i + 1))
    done
}
wait_gone() {
    i=0; while [ -n "$(state "$1")" ] && [ $i -lt 60 ]; do sleep 1; i=$((i + 1)); done
}
# drive NAME: what the agent does with a machine it manages.
drive() {
    run machinectl show "$1" -p State --value
    run machinectl --quiet shell "$1" /bin/sh -c "echo shell-ok"
    run systemd-run --machine="$1" --pipe --wait --quiet cat /etc/os-release
    run machinectl enable "$1"
    run machinectl disable "$1"
    if run machinectl poweroff "$1"; then wait_gone "$1"; fi
    if run machinectl start "$1"; then wait_running "$1"; fi
    if run machinectl terminate "$1"; then wait_gone "$1"; fi
    if run machinectl start "$1"; then wait_running "$1"; fi
}

echo "  -- through dbus-daemon"
run machinectl list --no-pager --no-legend
run busctl --system --no-pager --no-legend list --acquired
run loginctl list-sessions --no-pager --no-legend
run hostnamectl --static
run resolvectl domain

# Through systemd'\''s private socket, which root'\''s systemctl and systemd-run use.
# systemd checks unit operations against the unit file'\''s label, so the unit is
# written as an agent installs one, into /etc/systemd/system.
echo "  -- units"
run systemctl is-system-running
printf "[Unit]\nDescription=selinux-repro test unit\n\n[Service]\nType=oneshot\nExecStart=/bin/true\n\n[Install]\nWantedBy=multi-user.target\n" \
    >"/host/etc/systemd/system/$UNIT"
label "/etc/systemd/system/$UNIT" "$UNIT"
run systemctl daemon-reload
run systemctl enable "$UNIT"
run systemctl is-enabled "$UNIT"
run systemctl start "$UNIT"
run systemctl restart "$UNIT"
run systemctl stop "$UNIT"
run systemctl reset-failed "$UNIT"
run systemctl disable "$UNIT"
run systemctl enable --now "$UNIT"
run systemctl disable --now "$UNIT"
if [ -n "$MASK_UNIT" ]; then
    run systemctl mask --now "$MASK_UNIT"
    run systemctl unmask "$MASK_UNIT"
fi
run systemd-run --quiet --collect --unit="$REPRO-timer" --on-active=1s /usr/bin/true

echo "  -- kernel and files"
run modprobe dummy
run ip link add "$IFACE" type dummy
run ip link del "$IFACE"
run nft add table inet acl_selinux_repro
run nft delete table inet acl_selinux_repro
run sysctl -w "net.ipv4.ip_forward=$(cat /proc/sys/net/ipv4/ip_forward)"
mkdir -p "/host/run/$REPRO-bpf"
run mount -t bpf bpf "/run/$REPRO-bpf"
run mountpoint "/run/$REPRO-bpf"
run umount "/run/$REPRO-bpf"
run udevadm info --query=property --name="/dev/$BLOCKDEV"
run swapoff -a
mkdir -p "/host/opt/$REPRO/bin" && echo test >"/host/opt/$REPRO/bin/file"
run restorecon -R "/opt/$REPRO"
mkdir -p "/host/var/lib/machines/$REPRO-img/usr"
run machinectl remove "$REPRO-img"

if [ -n "$MACHINE" ]; then
    echo "  -- the existing machine $MACHINE"
    drive "$MACHINE"

    # The agent unpacks a machine'\''s root filesystem from OCI layers, which carry
    # no SELinux labels, so its files get the label of whatever creates them.
    # The copy keeps no extended attributes, for the same result.
    echo "  -- a machine this container creates, $NEW_MACHINE"
    run cp -dR --preserve=mode,ownership,timestamps,links "/var/lib/machines/$MACHINE" "/var/lib/machines/$NEW_MACHINE"
    # Two machines with one machine ID would clash in machined.
    if [ -s "/host/var/lib/machines/$NEW_MACHINE/etc/machine-id" ]; then
        : >"/host/var/lib/machines/$NEW_MACHINE/etc/machine-id"
    fi
    if [ -f "/host/etc/systemd/nspawn/$MACHINE.nspawn" ]; then
        cp "/host/etc/systemd/nspawn/$MACHINE.nspawn" "/host/etc/systemd/nspawn/$NEW_MACHINE.nspawn"
    fi
    label "/var/lib/machines/$NEW_MACHINE/usr/lib/systemd/systemd" "its init"
    if run machinectl start "$NEW_MACHINE"; then
        wait_running "$NEW_MACHINE"
        leader=$(chroot /host /usr/bin/machinectl show "$NEW_MACHINE" -p Leader --value 2>/dev/null)
        echo "  new machine context: $(cat "/proc/$leader/attr/current" 2>/dev/null)"
        drive "$NEW_MACHINE"
        if run machinectl terminate "$NEW_MACHINE"; then wait_gone "$NEW_MACHINE"; fi
    fi
    run machinectl remove "$NEW_MACHINE"
fi
'

# probe NAME [ctr run options...]: run the checks in a privileged container with
# the host's root at /host and the host's network and PID namespaces, list the
# SELinux denials logged meanwhile, put the host back, and add a summary line.
# dbus-daemon's denials reach the journal through the audit system, a little
# behind, so it waits for them before counting.
probe() {
    local name=$1 since until out denials context machine_context checks failed kinds
    shift
    since=$(date '+%Y-%m-%d %H:%M:%S')
    sleep 1
    out=$(ctr -n "$NS" run --rm "$@" --privileged --net-host --with-ns pid:/proc/1/ns/pid \
        --mount type=bind,src=/,dst=/host,options=rbind:rslave \
        --env "MACHINE=$MACHINE" --env "REPRO=$REPRO" --env "IFACE=$IFACE" --env "UNIT=$UNIT" \
        --env "NEW_MACHINE=$NEW_MACHINE" --env "MASK_UNIT=$MASK_UNIT" --env "BLOCKDEV=$blockdev" \
        "$IMAGE" probe sh -c "$PROBE" </dev/null 2>&1 || true)
    printf '%s\n' "$out"
    sleep 3
    until=$(date '+%Y-%m-%d %H:%M:%S')
    sleep 1
    denials=$(journalctl --since "$since" --until "$until" --no-pager -o cat | grep 'avc:  denied' |
        sed -E 's/.*avc:  denied  //; s/ (pid|spid|ino|uid|auid|ses|sauid)=[0-9]+//g; s/ (exe|hostname|addr|terminal)=.*//' |
        sort | uniq -c || true)
    echo "  SELinux denials: ${denials:-none}"
    reset_host >/dev/null 2>&1 || true

    context=$(sed -n 's/.*container context: system_u:system_r:\([a-z_]*\):.*/\1/p' <<<"$out")
    machine_context=$(sed -n 's/.*new machine context: system_u:system_r:\([a-z_]*\):.*/\1/p' <<<"$out")
    checks=$(grep -c ' exit [0-9]' <<<"$out" || true)
    failed=$(grep -c ' exit [1-9]' <<<"$out" || true)
    kinds=$(awk '
        match($0, /\{ [^}]* \}/) {
            perm = substr($0, RSTART + 2, RLENGTH - 4); gsub(/ /, ",", perm)
            class = $0; sub(/.*tclass=/, "", class); sub(/ .*/, "", class)
            n[class ":" perm] += $1
        }
        END { for (k in n) printf "%s=%d ", k, n[k] }' <<<"$denials" | sed 's/ $//')
    summary+=("$(printf '%-40s %-9s %-11s %-9s %s' "$name" "${context:-?}" "${machine_context:--}" "$failed/$checks" "${kinds:-none}")")
}

section "Environment"
# In a subshell: os-release sets NAME, VERSION, and more.
# shellcheck source=/dev/null
echo "OS          $(. /etc/os-release && echo "$PRETTY_NAME (VERSION=$VERSION_ID BUILD_ID=${BUILD_ID:-unknown})")"
echo "kernel      $(uname -r)"
echo "systemd     $(systemctl --version | head -1)"
echo "containerd  $(containerd --version | cut -d' ' -f3)"
echo "runc        $(runc --version | head -1)"
echo "SELinux     $(getenforce), $policy_file ($(sha256sum <"$policy_file" | cut -c1-16)), policy store: $([[ -d /var/lib/selinux ]] && echo present || echo none)"
echo "/usr        $(findmnt -n -o FSTYPE,OPTIONS /usr)"

section "Host binaries and unit files: label in file_contexts vs. actual label"
printf '%-46s %-28s %s\n' FILE EXPECTED ACTUAL
for f in /usr/lib/systemd/systemd /usr/bin/dbus-daemon /usr/lib/systemd/systemd-machined \
    /usr/lib/systemd/systemd-logind /usr/bin/containerd /usr/bin/runc /usr/bin/machinectl \
    /usr/lib/systemd/system/systemd-nspawn@.service /etc/systemd/system /var/lib/machines; do
    printf '%-46s %-28s %s\n' "$f" "$(type_of "$(matchpathcon -n "$f")")" "$(type_of "$(stat -c %C "$f")")"
done

section "Host processes"
machinectl list >/dev/null 2>&1 || true  # starts machined if it is not running
printf '%-18s %s\n' systemd "$(tr -d '\0' </proc/1/attr/current)"
for comm in dbus-daemon systemd-machine systemd-logind systemd-resolve containerd; do
    pid=$(pgrep -o -x "$comm" || true)
    [[ -n $pid ]] && printf '%-18s %s\n' "$comm" "$(tr -d '\0' <"/proc/$pid/attr/current")"
done
if [[ -n $MACHINE ]]; then
    leader=$(machinectl show "$MACHINE" -p Leader --value)
    printf '%-18s %s\n' "machine $MACHINE" "$(tr -d '\0' <"/proc/$leader/attr/current")"
fi

ctr -n "$NS" image pull "$IMAGE" >/dev/null

section "Privileged container, default snapshotter (overlayfs), current policy"
probe "current policy"

for candidate in "${candidate_policies[@]}"; do
    section "Privileged container, default snapshotter, policy $candidate"
    policy_backup=$(mktemp /root/policy.orig.XXXXXX)
    cp "$policy_file" "$policy_backup"
    cp "$candidate" "$policy_file"
    load_policy
    echo "  loaded $candidate ($(sha256sum <"$policy_file" | cut -c1-16))"
    probe "policy $(basename "$candidate")"
    restore_policy
done

if (( workaround )); then
    section "Workaround: native snapshotter, its directory relabeled usr_t"
    mkdir -p "$NATIVE/snapshots"
    native_labels=("$(stat -c %C "$NATIVE")" "$(stat -c %C "$NATIVE/snapshots")")
    chcon -t usr_t "$NATIVE" "$NATIVE/snapshots"
    # ctr can log a harmless "context canceled" after a --local pull; the exit
    # status is what counts.
    ctr -n "$NS" image pull --local --snapshotter native "$IMAGE" >/dev/null 2>&1 ||
        { echo "could not pull $IMAGE for the native snapshotter" >&2; exit 1; }
    newest=$(find "$NATIVE/snapshots" -mindepth 1 -maxdepth 1 -printf '%T@ %p\n' | sort -n | tail -1 | cut -d' ' -f2)
    echo "  new snapshot $newest: $(stat -c %C "$newest")"
    probe "workaround (native snapshotter, usr_t)" --snapshotter native
fi

section "Summary"
printf '%-40s %-9s %-11s %-9s %s\n' PROBE CONTEXT NEW-MACHINE FAILED DENIALS
printf '%s\n' "${summary[@]}"
