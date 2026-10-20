#!/bin/bash
# Verify which SELinux policy rules Azure Container Linux needs for a privileged
# container to manage host services. See SELINUX.md next to this file.
#
# Run it on a machine with podman and SSH access to an ACL host, as a user who
# can sudo there:
#
#   ACL_SSH="ssh core@ACL_HOST" ./selinux-verify.sh
#   ACL_SSH="ssh core@ACL_HOST" MACHINE=demo ./selinux-verify.sh
#
# ACL_SSH is a command that runs its last argument on the host, passing stdin
# through. MACHINE names a running systemd-nspawn machine on the host, and adds
# the machine checks. The script:
#
#   1. copies the host's compiled policy
#   2. builds one candidate policy per rule set in CANDIDATES below, in a
#      Fedora container with checkpolicy, secilc, and setools, and checks that
#      each differs from the host's policy by exactly its rules
#   3. runs selinux-repro.sh on the host: the current policy, each candidate,
#      and the workaround. Each candidate is loaded only for its probe, and the
#      original policy reloaded after it.
#
# The candidates and the results are kept in WORK, a new temporary directory
# by default.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
: "${ACL_SSH:?set ACL_SSH to a command that runs a command on the ACL host, such as \"ssh core@ACL_HOST\"}"
MACHINE=${MACHINE:-}
FEDORA_IMAGE=${FEDORA_IMAGE:-registry.fedoraproject.org/fedora:44}
work=${WORK:-$(mktemp -d /tmp/acl-selinux-verify.XXXXXX)}
# A directory of its own on the host, made by mktemp below, from which root runs
# selinux-repro.sh: a fixed path in /tmp could be another user's.
remote=

# name|CIL rules, one candidate per line, each on top of the host's policy. Add
# lines to compare other rule sets. Against ACL 3.0.20261005, adding
# (allow spc_t unlabeled_t (service (enable disable))) as well changed nothing:
# enabling and disabling units already works from spc_t.
CANDIDATES=(
    'dbus|(allow spc_t kernel_t (dbus (send_msg)))'
)

read -ra ssh_cmd <<<"$ACL_SSH"
host() { "${ssh_cmd[@]}" "$1"; }
trap '[[ -z $remote ]] || host "rm -rf $remote" >/dev/null 2>&1 || true' EXIT

echo "== Copying the host's policy into $work"
mkdir -p "$work"
# These expand on the host.
# shellcheck disable=SC2016
policy_name=$(host 'sudo sh -c ". /etc/selinux/config && cd /etc/selinux/\$SELINUXTYPE/policy && echo policy.*"')
# One name, or the check below refuses it, as selinux-repro.sh does; see there.
if [[ ! $policy_name =~ ^policy\.[0-9]+$ ]]; then
    echo "expected one policy file on the host, found: $policy_name" >&2
    exit 1
fi
# shellcheck disable=SC2016
host 'sudo sh -c ". /etc/selinux/config && cat /etc/selinux/\$SELINUXTYPE/policy/'"$policy_name"'"' >"$work/$policy_name"
version=${policy_name#policy.}
echo "$policy_name, $(sha256sum <"$work/$policy_name" | cut -c1-16)"

names=()
for candidate in "${CANDIDATES[@]}"; do
    name=${candidate%%|*}
    names+=("$name")
    printf '%s\n' "${candidate#*|}" >"$work/rules-$name.cil"
done

echo
echo "== Building and checking the candidates"
podman run --rm -v "$work:/work:z" -e "POLICY=$policy_name" -e "VERSION=$version" \
    -e "NAMES=${names[*]}" "$FEDORA_IMAGE" bash -c '
set -euo pipefail
dnf -q -y install checkpolicy secilc python3-setools >/dev/null 2>&1
cd /work
read -r mls unknown < <(python3 -c "
import setools
p = setools.SELinuxPolicy(\"$POLICY\")
print(str(p.mls).lower(), p.handle_unknown)")
checkpolicy $([ "$mls" = true ] && echo -M) -b -C -o policy.cil "$POLICY"
for name in $NAMES; do
    { cat policy.cil; echo; cat "rules-$name.cil"; } >"candidate-$name.cil"
    secilc -M "$mls" -c "$VERSION" -U "$unknown" -f /dev/null -o "candidate-$name.policy" "candidate-$name.cil"
done
python3 - "$POLICY" $NAMES <<EOF
import sys
import setools
from setools.diff import PolicyDifference

original = setools.SELinuxPolicy(sys.argv[1])
ok = True
for name in sys.argv[2:]:
    diff = PolicyDifference(original, setools.SELinuxPolicy(f"candidate-{name}.policy"))
    grants = [str(r) for r in diff.added_allows]
    # An existing rule given more permissions is a modification, not an addition.
    for m in diff.modified_allows:
        if m.removed_perms:
            ok = False
            print(f"  unexpected: {m.rule} loses {sorted(m.removed_perms)}")
        rule = m.rule
        added = " ".join(sorted(m.added_perms))
        kept = " ".join(sorted(m.matched_perms))
        grants.append(f"allow {rule.source} {rule.target}:{rule.tclass} {{ {added} }};"
                      f"  (added to the existing {{ {kept} }})")
    other = {attr: list(getattr(diff, attr)) for attr in (
        "removed_allows", "added_types", "removed_types",
        "added_type_transitions", "removed_type_transitions", "added_booleans",
        "removed_booleans", "added_classes", "removed_classes", "added_roles",
        "removed_roles") if getattr(diff, attr)}
    print(f"candidate-{name}.policy grants, over the host policy:")
    for rule in sorted(grants):
        print(f"  {rule}")
    for attr, items in other.items():
        ok = False
        print(f"  unexpected {attr}: {items[:5]}")
sys.exit(0 if ok else 1)
EOF
'

echo
echo "== Running selinux-repro.sh on the host"
remote=$(host "mktemp -d /tmp/acl-selinux-verify.XXXXXX")
host "cat >$remote/selinux-repro.sh" <"$here/selinux-repro.sh"
args=()
for name in "${names[@]}"; do
    host "cat >$remote/candidate-$name.policy" <"$work/candidate-$name.policy"
    args+=(--policy "$remote/candidate-$name.policy")
done
host "sudo MACHINE=$(printf %q "$MACHINE") bash $remote/selinux-repro.sh ${args[*]} --workaround" |
    tee "$work/results.txt"
echo
echo "candidates and results: $work"
