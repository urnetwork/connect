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

# --system: the rendered system unit, which grants the one capability
# (Provider_Install_Linux.sh render_system_service and its restart units)
sh -n "$here/Provider_Install_Linux.sh" || fail "the installer doesn't parse"
awk '/^(unit_path|render_system_service|render_system_restart_path|render_system_restart_service|system_unit_ours|want_system_service|extender_bind_hint|as_root|unit_active|unit_enabled|setup_system_service|write_systemd_units|interval_oncalendar|pr_err|pr_info) \(\)$/ { on=1 } on { print } on && /^}$/ { on=0 }' \
    "$here/Provider_Install_Linux.sh" > "$run_dir/system.sh"
marker="$(sed -n 's/^system_unit_marker="\(.*\)"$/\1/p' "$here/Provider_Install_Linux.sh")"
[ "$marker" = "# Installed by urnet-tools --system" ] || fail "no system unit marker: '$marker'"

render ()
{
    install_path="$1" system_unit_marker="$marker" sh -c ". '$run_dir/system.sh'; $2"
}

render "/home/alice/.local/share/urnetwork-provider" "render_system_service alice" > "$run_dir/unit" ||
    fail "render_system_service failed"
cat > "$run_dir/unit.want" <<'UNIT'
# Installed by urnet-tools --system. Remove with: urnet-tools uninstall
[Unit]
Description=URnetwork Provider
Documentation=https://ur.xyz/docs/miner/extender
Wants=network-online.target
After=network-online.target

[Service]
User=alice
ExecStart="/home/alice/.local/share/urnetwork-provider/bin/urnetwork" provide
Restart=on-failure
RestartSec=30
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=yes

[Install]
WantedBy=multi-user.target
UNIT
cmp -s "$run_dir/unit" "$run_dir/unit.want" ||
    fail "system unit differs: $(diff "$run_dir/unit.want" "$run_dir/unit")"

render "/home/alice/50% share" "render_system_restart_path" > "$run_dir/path"
grep -qx 'PathChanged=/home/alice/50%% share/.version' "$run_dir/path" ||
    fail "restart path doesn't watch .version: $(cat "$run_dir/path")"
render "/x" "render_system_restart_service /usr/bin/systemctl" > "$run_dir/restart"
grep -qx 'ExecStart=/usr/bin/systemctl try-restart urnetwork.service' "$run_dir/restart" ||
    fail "restart service: $(cat "$run_dir/restart")"
grep -q '^User=' "$run_dir/restart" && fail "the root restart unit names a user"

# --system: which install runs the provider as a system service
cat > "$run_dir/id" <<STUB
#!/bin/sh
case "\$1" in
    -u) cat "$run_dir/uid" ;;
    -un) cat "$run_dir/uname_" ;;
esac
STUB
chmod +x "$run_dir/id"
mkdir -p "$run_dir/booted"

# $1 uid, $2 user, $3 --system, $4 operation, $5 no_user_manager, $6 shell code
pick ()
{
    echo "$1" > "$run_dir/uid"
    echo "$2" > "$run_dir/uname_"
    PATH="$run_dir:$PATH" install_path=/x system_unit_marker="$marker" system_service="$3" operation="$4" \
        no_user_manager="$5" system_service_file="$run_dir/sys/urnetwork.service" \
        systemd_booted_dir="$run_dir/booted" sh -c ". '$run_dir/system.sh'; $6"
}

mkdir -p "$run_dir/sys"
rm -f "$run_dir/sys/urnetwork.service"
pick 1000 alice 0 install 0 want_system_service && fail "a user install became a system service unasked"
[ "$(pick 1000 alice 0 install 0 extender_bind_hint)" = "urnet-tools reinstall --system" ] ||
    fail "a user service doesn't say how to bind TCP 443"
pick 1000 alice 1 install 0 want_system_service || fail "--system was not taken"
pick 1000 alice 1 reinstall 0 want_system_service || fail "reinstall --system was not taken"
pick 0 root 0 install 1 want_system_service || fail "root without a user session got no system service"
pick 0 root 0 update 1 want_system_service && fail "an update changed a root install into a system service"
pick 0 root 0 install 0 want_system_service && fail "root with a user session lost its user service"
[ -z "$(pick 0 root 0 install 0 extender_bind_hint)" ] || fail "root was told to get a privilege it has"
rmdir "$run_dir/booted"
pick 0 root 0 install 1 want_system_service && fail "a system service without systemd as init"
mkdir -p "$run_dir/booted"

# an installed system unit keeps the mode for its own user, through updates
pick 1000 alice 0 install 0 "render_system_service alice" > "$run_dir/sys/urnetwork.service"
pick 1000 alice 0 update 0 want_system_service || fail "an update of a system service went back to a user service"
[ -z "$(pick 1000 alice 0 update 0 extender_bind_hint)" ] || fail "a system service was told to get the privilege"
pick 1001 bob 0 update 0 want_system_service && fail "another user's system unit was taken as this user's"
sed 1d "$run_dir/sys/urnetwork.service" > "$run_dir/sys/handmade"
mv "$run_dir/sys/handmade" "$run_dir/sys/urnetwork.service"
pick 1000 alice 0 update 0 want_system_service && fail "a hand-written urnetwork.service was taken as the installer's"

# --system: the switch never leaves the user unit and the system unit both on
home="$run_dir/switch"
mkdir -p "$home/user" "$home/sys" "$home/work" "$home/booted"
printf '[Unit]\n' > "$home/user/urnetwork.service"
: > "$home/calls"
echo active > "$home/user-active"
cat > "$run_dir/systemctl" <<STUB
#!/bin/sh
echo "systemctl \$*" >> "$home/calls"
case "\$*" in
    "--user is-active --quiet urnetwork.service") [ "\$(cat "$home/user-active")" = active ]; exit \$? ;;
    "--user is-enabled urnetwork.service") echo enabled ;;
    "--user disable --now urnetwork.service") echo inactive > "$home/user-active" ;;
    "is-active --quiet urnetwork.service") exit 3 ;;
esac
exit 0
STUB
cat > "$run_dir/sudo" <<STUB
#!/bin/sh
echo "sudo \$*" >> "$home/calls"
exec "\$@"
STUB
chmod +x "$run_dir/systemctl" "$run_dir/sudo"
echo 1000 > "$run_dir/uid"
echo alice > "$run_dir/uname_"
PATH="$run_dir:$PATH" me=urnet-tools operation=reinstall install_path=/home/alice/.local/share/urnetwork-provider \
    system_unit_marker="$marker" system_service=1 has_systemd=1 no_user_manager=0 service_stopped=0 \
    workdir="$home/work" systemd_service="$home/user/urnetwork.service" system_unit_dir="$home/sys" \
    system_service_file="$home/sys/urnetwork.service" system_restart_path="$home/sys/urnetwork-restart.path" \
    system_restart_service="$home/sys/urnetwork-restart.service" systemd_booted_dir="$home/booted" \
    sh -c ". '$run_dir/system.sh'; setup_system_service; echo \"stopped=\$service_stopped\"" > "$home/out" 2>&1 ||
    fail "setup_system_service failed: $(cat "$home/out")"
[ -e "$home/user/urnetwork.service" ] && fail "the user's urnetwork.service is still there beside the system unit"
grep -qx "systemctl --user disable --now urnetwork.service" "$home/calls" || fail "the user unit was not stopped: $(cat "$home/calls")"
grep -q "^sudo install -m 644 .*/urnetwork.service $home/sys/urnetwork.service$" "$home/calls" || fail "no sudo install: $(cat "$home/calls")"
grep -qx "sudo systemctl enable --now urnetwork-restart.path" "$home/calls" || fail "the restart path was not turned on: $(cat "$home/calls")"
grep -qx "sudo systemctl enable urnetwork.service" "$home/calls" || fail "the system unit doesn't start at boot like the user unit did: $(cat "$home/calls")"
grep -qx "stopped=1" "$home/out" || fail "the running provider is not started again as the system service: $(cat "$home/out")"
grep -qx "User=alice" "$home/sys/urnetwork.service" || fail "the system unit doesn't run as alice"
# the user units the update rewrites leave the provider to the system unit
PATH="$run_dir:$PATH" me=urnet-tools operation=update install_path=/x system_unit_marker="$marker" system_service=0 \
    no_user_manager=0 systemd_userdir="$home/user" systemd_service="$home/user/urnetwork.service" \
    systemd_update_service="$home/user/urnetwork-update.service" systemd_update_timer="$home/user/urnetwork-update.timer" \
    system_service_file="$home/sys/urnetwork.service" systemd_booted_dir="$home/booted" \
    sh -c ". '$run_dir/system.sh'; write_systemd_units 6h" > "$home/out" 2>&1 || fail "write_systemd_units: $(cat "$home/out")"
[ -e "$home/user/urnetwork.service" ] && fail "an update wrote the user's urnetwork.service beside the system unit"
[ -e "$home/user/urnetwork-update.timer" ] || fail "an update dropped the update timer"
rm -f "$run_dir/systemctl" "$run_dir/sudo" "$run_dir/id"

echo "Provider_Install_Linux: OK"
