#!/bin/sh
# Proves the installer keeps the provider's systemd user service alive after
# logout: lingering must end up on, or the installer must say it is not.
# Without lingering the user manager stops urnetwork.service at logout and
# starts it again at the next login.
set -eu

here="$(cd "$(dirname "$0")" && pwd)"
run_dir="$(mktemp -d "${TMPDIR:-/tmp}/urnetwork-linger.test.XXXXXX")"
trap 'rm -rf "$run_dir"' EXIT

# only the functions under test, never the installer's main flow
awk '/^(pr_err|pr_info|enable_lingering) \(\)$/ { on=1 } on { print } on && /^}$/ { on=0 }' \
    "$here/Provider_Install_Linux.sh" > "$run_dir/functions.sh"

fail ()
{
    echo "FAIL: $*" >&2
    exit 1
}

# $1 initial Linger value, $2 whether enable-linger succeeds, $3 has_systemd
run_case ()
{
    printf '%s\n' "$1" > "$run_dir/linger"
    : > "$run_dir/calls"
    cat > "$run_dir/loginctl" <<STUB
#!/bin/sh
echo "\$*" >> "$run_dir/calls"
case "\$1" in
    show-user) cat "$run_dir/linger" ;;
    enable-linger) [ "$2" = ok ] || exit 1; echo yes > "$run_dir/linger" ;;
esac
STUB
    chmod +x "$run_dir/loginctl"
    PATH="$run_dir:$PATH" me=urnet-tools operation=install has_systemd="$3" \
        sh -c ". '$run_dir/functions.sh'; enable_lingering" > "$run_dir/out" 2>&1 ||
        fail "enable_lingering must not abort the install: $(cat "$run_dir/out")"
}

run_case yes ok 1
grep -q enable-linger "$run_dir/calls" && fail "lingering already on was enabled again"
grep -q warning "$run_dir/out" && fail "warned although lingering is on"

run_case no ok 1
grep -q enable-linger "$run_dir/calls" || fail "lingering was not enabled"
grep -q warning "$run_dir/out" && fail "warned although enabling lingering succeeded"

run_case no fail 1
grep -q "warning:.*log out" "$run_dir/out" || fail "failed enable-linger was not reported: $(cat "$run_dir/out")"
grep -q "sudo loginctl enable-linger" "$run_dir/out" || fail "no remedy printed: $(cat "$run_dir/out")"

run_case no ok 0
[ -s "$run_dir/calls" ] && fail "loginctl was called without systemd"

echo "Provider_Install_Linux: OK"
