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

# the release architecture for each `uname -m'
awk '/^get_arch \(\)$/ { on=1 } on { print } on && /^}$/ { on=0 }' \
    "$here/Provider_Install_Linux.sh" > "$run_dir/arch.sh"

for pair in x86_64:amd64 i686:386 aarch64:arm64 armv7l:arm armv8l:arm armv6l: mips:; do
    printf '#!/bin/sh\necho %s\n' "${pair%%:*}" > "$run_dir/uname"
    chmod +x "$run_dir/uname"
    got="$(PATH="$run_dir:$PATH" sh -c ". '$run_dir/arch.sh'; get_arch")"
    [ "$got" = "${pair#*:}" ] || fail "uname -m ${pair%%:*} gave '$got', want '${pair#*:}'"
done
rm -f "$run_dir/uname"

# the update interval survives a round trip through the timer unit
awk '/^(current_interval|interval_oncalendar) \(\)$/ { on=1 } on { print } on && /^}$/ { on=0 }' \
    "$here/Provider_Install_Linux.sh" > "$run_dir/interval.sh"

for interval in 6h daily weekly monthly; do
    got="$(systemd_update_timer="$run_dir/timer" sh -c ". '$run_dir/interval.sh'
        printf '[Timer]\nOnCalendar=%s\n' \"\$(interval_oncalendar $interval)\" > \"\$systemd_update_timer\"
        current_interval")"
    [ "$got" = "$interval" ] || fail "interval $interval read back as '$got'"
done

# options work in any position and in both `--opt value' and `--opt=value' form
run_installer ()
{
    HOME="$run_dir/home" URNETWORK_API_BASE="http://127.0.0.1:9/none" \
        sh "$here/Provider_Install_Linux.sh" "$@" > "$run_dir/out" 2>&1
}

mkdir -p "$run_dir/home"

for args in "-t 1.2 -B --no-start" "--tag=1.2 --no-modify-bashrc" "-B -t=1.2 --interval=weekly" "--interval weekly --tag 1.2"; do
    # shellcheck disable=SC2086
    run_installer $args && fail "install with no release server succeeded"
    grep -q "Fetching release information for tag: v1.2" "$run_dir/out" ||
        fail "options '$args' were not accepted: $(cat "$run_dir/out")"
done

run_installer --tag= && fail "an empty --tag= was accepted"
grep -q "requires an argument" "$run_dir/out" || fail "empty --tag= not reported: $(cat "$run_dir/out")"

run_installer --interval hourly && fail "an unknown interval was accepted"
grep -q "Invalid update interval" "$run_dir/out" || fail "bad interval not reported: $(cat "$run_dir/out")"

run_installer --purge && fail "an uninstall option was accepted by install"
grep -q "doesn't apply" "$run_dir/out" || fail "--purge on install not reported: $(cat "$run_dir/out")"

[ -e "$run_dir/home/.local/share/urnetwork-provider" ] && fail "a failed install left files behind"

echo "Provider_Install_Linux: OK"
