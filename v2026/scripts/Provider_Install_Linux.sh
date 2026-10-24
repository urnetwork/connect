#!/bin/sh
# Credits: Ar Rakin, Ryan Mello
# urnet-tools -- URnetwork manager script (also acts as an installation script)
# GitHub: <https://github.com/urnetwork/connect>
#
# Install:  curl -fsSL https://raw.githubusercontent.com/urnetwork/connect/refs/heads/main/scripts/Provider_Install_Linux.sh | sh
# Options:  ... | sh -s -- --tag v2026.1.2 --no-modify-bashrc
me="$0"
script_rundir="$(pwd)"

case "$(basename -- "$me" 2>/dev/null)" in
    sh|bash|dash|zsh|ksh|busybox)
        me="urnet-tools"
        ;;
esac

if [ "$URNETWORK_TOOLS_MODE" = "1" ]; then
    me="urnet-tools"
fi

default_script_url="https://raw.githubusercontent.com/urnetwork/connect/refs/heads/main/scripts/Provider_Install_Linux.sh"
# where `urnet-tools update` fetches newer copies of itself; baked into the
# installed urnet-tools so a fork or mirror keeps updating from where it came
script_url="${URNETWORK_TOOLS_URL:-${URNETWORK_TOOLS_SCRIPT_URL:-$default_script_url}}"
# raise with every change to this script: `update' never installs a urnet-tools
# with a lower number, so a revert or an old mirror can't downgrade installs
tools_version=2
api_base="${URNETWORK_API_BASE:-https://api.github.com/repos/urnetwork/build}"
default_interval="6h"
# the service runs without URNETWORK_STATE_DIR, so the miner this script runs
# (sign-in, first-start registration) must use the same ~/.urnetwork
state_dir="$HOME/.urnetwork"
unset URNETWORK_STATE_DIR

show_help ()
{
    echo "Usage: "

    if [ -n "$URNETWORK_TOOLS_MODE" ]; then
        echo "  $me [options] start"
        echo "  $me [options] stop"
        echo "  $me [options] status"
        echo "  $me [options] logs"
        echo "  $me [options] update"
        echo "  $me [options] reinstall [--tag TAG] [--no-modify-bashrc]"
        echo "  $me [options] restart"
        echo "  $me [options] version"
        echo "  $me [options] uninstall [--remove-identity [--yes]] [--no-modify-bashrc]"
        echo "  $me [options] auto-update [on | off] [--interval INTERVAL]"
        echo "  $me [options] auto-start [on | off]"
        echo "  $me [-h] [-v]"
    else
        echo "  curl -fsSL <url> | sh -s -- [options]"
        echo "  $me [options]"
    fi

    echo ""

    if [ -z "$URNETWORK_TOOLS_MODE" ]; then
        echo "Installs URnetwork locally for the current user. No sudo needed."
    else
        echo "Manages URnetwork installation."
    fi

    if [ -n "$URNETWORK_TOOLS_MODE" ]; then
        echo ""
        echo "Operational modes:"
        echo "  start                   Start the URnetwork provider service"
        echo "  stop                    Stop the URnetwork provider service"
        echo "  restart                 Restart the URnetwork provider service"
        echo "  status                  Show whether the provider runs, is signed in, and updates."
        echo "                          Exits 0 when it runs, 3 when it doesn't"
        echo "  version                 Show the installed and the latest version"
        echo "  logs                    Follow the provider service log (Ctrl-C to leave)"
        echo "  update                  Upgrade URnetwork and urnet-tools, if updates are available"
        echo "  reinstall               Reinstall URnetwork"
        echo "  uninstall               Uninstall URnetwork. Your miner identity in ~/.urnetwork"
        echo "                          is kept unless you add --remove-identity"
        echo "  auto-update             Turn automatic updates on or off.  With no argument,"
        echo "                          prints the current state and interval."
        echo "  auto-start              Turn starting the provider at boot/login on or off"
        echo ""
        echo "Options for reinstall:"
        echo "  -t, --tag TAG           Reinstall a specific version of URnetwork"
        echo "  -B, --no-modify-bashrc  Do not modify ~/.bashrc"
        echo ""
        echo "Options for uninstall:"
        echo "  --remove-identity       Also delete ~/.urnetwork (this device's miner identity)"
        echo "  -y, --yes               Don't ask before deleting it"
        echo "  -B, --no-modify-bashrc  Do not modify ~/.bashrc"
        echo ""
        echo "Options for auto-update:"
        echo "  --interval INTERVAL     How often to check: 6h, daily, weekly or monthly."
        echo "                          Alone, changes the interval and keeps updates on or off."
        echo "                          Keeps the current interval if left out ($default_interval on a new install)."
    else
        echo ""
        echo "Options:"
        echo "  -t, --tag TAG           Install a specific version of URnetwork"
        echo "  -B, --no-modify-bashrc  Do not add urnetwork to PATH in ~/.bashrc"
        echo "  --interval INTERVAL     Automatic update interval: 6h (default), daily, weekly, monthly"
        echo "  --auth-code CODE        Sign in with this auth code (or set URNETWORK_AUTH_CODE)."
        echo "                          Without it, the installer asks when run in a terminal."
        echo "  --no-start              Install only: don't sign in or start the provider"
        echo "  -y, --yes               Don't ask anything (sign in only with --auth-code)"
    fi

    echo ""
    echo "Global Options:"
    echo "  -h, --help              Show this help and exit"
    echo "  -v, --version           Show the version of URnetwork that's installed"
    echo "  -i, --install PATH      Installation path"
    echo ""
    echo "Options take their value either as '--tag v1.2.3' or '--tag=v1.2.3'."
    echo "Refer to <https://ur.xyz/docs/miner> for more help."
}

# prints the release architecture for this machine, or nothing if no
# release build runs here
get_arch ()
{
    case "$(uname -m)" in
        i386|i486|i586|i686)
            echo 386
            ;;

        x86_64|amd64)
            echo amd64
            ;;

        # a 64-bit kernel runs the static arm64 build even on a 32-bit userland
        aarch64|arm64)
            echo arm64
            ;;

        # armv8l is a 32-bit process on a 64-bit ARM kernel
        armv7*|armv8l)
            echo arm
            ;;
    esac
}

operation=""
has_systemd=0
no_user_manager=0

install_path="${URNETWORK_INSTALL_PATH:-${URNETWORK_PROVIDER_INSTALL:-$HOME/.local/share/urnetwork-provider}}"

if [ -z "$URNETWORK_TOOLS_MODE" ]; then
    operation="install"
fi

bold=""
plain=""

if [ -t 1 ]; then
    bold="$(printf '\033[1m')"
    plain="$(printf '\033[0m')"
fi

pr_err ()
{
    argv0="$me"
    fmt="$1"
    shift

    if [ -t 2 ]; then
        argv0="\033[1m$me\033[0m"
    fi

    if [ -n "$operation" ]; then
        argv0="$argv0: $operation"
    fi

    # shellcheck disable=SC2059
    printf "$argv0: $fmt\n" "$@" >&2
}

pr_info ()
{
    argv0="$me"
    fmt="$1"
    shift

    if [ -t 1 ]; then
        argv0="\033[1m$me\033[0m"
    fi

    if [ -n "$operation" ]; then
        argv0="$argv0: $operation"
    fi

    # shellcheck disable=SC2059
    printf "$argv0: $fmt\n" "$@"
}

opt_requires_arg ()
{
    pr_err "Option '%s' requires an argument" "$1"
    pr_err "Try '%s --help' for more information" "$me"
}

# single-quotes $1 for use in shell code
sh_quote ()
{
    printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"
}

network_fetch ()
{
    if command -v curl > /dev/null; then
        curl -fsSL "$1"
        return $?
    elif command -v wget > /dev/null; then
        wget -qO- "$1"
        return $?
    else
        pr_err "Neither curl nor wget is available. Install one, e.g. \`sudo apt install curl'"
        exit 1
    fi
}

require_json_tool ()
{
    if command -v jq > /dev/null || command -v python3 > /dev/null; then
        return 0
    fi

    pr_err "Neither jq nor python3 is available; one is needed to read release information"
    pr_err "Install one, e.g. \`sudo apt install jq' or \`sudo dnf install jq', then run this again"
    exit 1
}

# $1 release json file; prints the release tag
release_tag ()
{
    if command -v jq > /dev/null; then
        tr -d '\000-\037' < "$1" | jq -r '.tag_name // empty'
    else
        tr -d '\000-\037' < "$1" | python3 -c 'import sys, json; print(json.load(sys.stdin).get("tag_name") or "")'
    fi
}

# $1 release json file; prints one "name<TAB>url<TAB>digest" line per asset
release_assets ()
{
    if command -v jq > /dev/null; then
        tr -d '\000-\037' < "$1" | jq -r '.assets[]? | [.name, .browser_download_url, (.digest // "")] | @tsv'
    else
        tr -d '\000-\037' < "$1" | python3 -c '
import sys, json
for a in json.load(sys.stdin).get("assets") or []:
    print("%s\t%s\t%s" % (a.get("name", ""), a.get("browser_download_url", ""), a.get("digest") or ""))
'
    fi
}

# $1 release json file, $2 asset name; prints the sha256 the release notes
# list for the asset (the |Asset|SHA256| table), if any
release_body_sha256 ()
{
    if command -v jq > /dev/null; then
        body="$(jq -r '.body // ""' < "$1")"
    else
        body="$(python3 -c 'import sys, json; print(json.load(sys.stdin).get("body") or "")' < "$1")"
    fi

    printf '%s\n' "$body" | grep -F "[$2](" | grep -oE '[0-9a-fA-F]{64}' | head -n1
}

file_sha256 ()
{
    if command -v sha256sum > /dev/null; then
        sha256sum "$1" | awk '{ print $1 }'
    elif command -v shasum > /dev/null; then
        shasum -a 256 "$1" | awk '{ print $1 }'
    elif command -v openssl > /dev/null; then
        openssl dgst -sha256 "$1" | awk '{ print $NF }'
    fi
}

installed_version ()
{
    if [ -f "$install_path/.version" ]; then
        cat "$install_path/.version"
    fi
}

show_version ()
{
    version="$(installed_version)"

    if [ -z "$version" ]; then
        pr_err "URnetwork is not installed in '%s'" "$install_path"
        exit 1
    fi

    echo "Current version: $version"

    release_json="$(mktemp)" || exit 1

    if command -v jq > /dev/null || command -v python3 > /dev/null; then
        if network_fetch "$api_base/releases/latest" > "$release_json" 2>/dev/null; then
            latest_version="$(release_tag "$release_json")"

            if [ -n "$latest_version" ] && [ "$latest_version" != "$version" ]; then
                echo "Latest version (Update available): $latest_version"
            fi
        fi
    fi

    rm -f "$release_json"
}

#
# Options. Every option is accepted in any position and with either
# `--opt value' or `--opt=value'; each operation then says which apply.
#

tag=""
no_modify_bashrc=0
interval=""
purge=0
assume_yes=0
auth_code="${URNETWORK_AUTH_CODE:-}"
no_start=0
want_version=0
used_opts=""
arg1=""
arg2=""

note_opt ()
{
    used_opts="$used_opts $1"
}

while [ $# -gt 0 ]; do
    opt="$1"
    val=""
    has_val=0

    case "$opt" in
        --*=*|-[ti]=*)
            val="${opt#*=}"
            opt="${opt%%=*}"
            has_val=1
            ;;
    esac

    case "$opt" in
        -i|--install|-t|--tag|--interval|--auth-code)
            if [ "$has_val" -eq 0 ]; then
                if [ $# -lt 2 ] || [ -z "$2" ]; then
                    opt_requires_arg "$opt"
                    exit 1
                fi

                val="$2"
                shift
            elif [ -z "$val" ]; then
                opt_requires_arg "$opt"
                exit 1
            fi
            ;;

        *)
            if [ "$has_val" -eq 1 ]; then
                pr_err "Option '%s' doesn't take a value" "$opt"
                exit 1
            fi
            ;;
    esac

    case "$opt" in
        -h|--help)
            show_help
            exit 0
            ;;

        -v|--version)
            want_version=1
            ;;

        -i|--install)
            install_path="$val"
            ;;

        -t|--tag)
            tag="$val"
            note_opt --tag
            ;;

        -B|--no-modify-bashrc|--no_modify_bashrc)
            no_modify_bashrc=1
            note_opt --no-modify-bashrc
            ;;

        --interval)
            case "$val" in
                6h|daily|weekly|monthly)
                    interval="$val"
                    ;;

                *)
                    pr_err "Invalid update interval '%s': must be one of 6h, daily, weekly, monthly" "$val"
                    exit 1
                    ;;
            esac
            note_opt --interval
            ;;

        --remove-identity|--purge)
            purge=1
            note_opt --remove-identity
            ;;

        -y|--yes)
            assume_yes=1
            note_opt --yes
            ;;

        --auth-code)
            auth_code="$val"
            note_opt --auth-code
            ;;

        --no-start)
            no_start=1
            note_opt --no-start
            ;;

        --)
            shift
            break
            ;;

        -*)
            pr_err "Invalid option '%s'" "$opt"
            pr_err "Try '%s --help' for more information" "$me"
            exit 1
            ;;

        *)
            if [ -z "$arg1" ]; then
                arg1="$opt"
            elif [ -z "$arg2" ]; then
                arg2="$opt"
            else
                pr_err "Unexpected argument '%s'" "$opt"
                exit 1
            fi
            ;;
    esac

    shift
done

for rest in "$@"; do
    if [ -z "$arg1" ]; then
        arg1="$rest"
    elif [ -z "$arg2" ]; then
        arg2="$rest"
    else
        pr_err "Unexpected argument '%s'" "$rest"
        exit 1
    fi
done

case "$install_path" in
    /*) ;;
    *) install_path="$script_rundir/$install_path" ;;
esac

case "$install_path" in
    *[\"\\\$\`]*)
        pr_err "The install path can't contain \", \\, \$ or \`: %s" "$install_path"
        exit 1
        ;;
esac

if [ "$want_version" -eq 1 ]; then
    show_version
    exit 0
fi

# in install mode a bare word is a mistake; in tools mode it is the operation
if [ -z "$URNETWORK_TOOLS_MODE" ]; then
    if [ -n "$arg1" ]; then
        pr_err "Unexpected argument '%s'" "$arg1"
        pr_err "Try '%s --help' for more information" "$me"
        exit 1
    fi
else
    operation="$arg1"
    arg1="$arg2"
    arg2=""
fi

if [ -z "$operation" ]; then
    show_help >&2
    exit 1
fi

# $@: the options this operation accepts
allow_opts ()
{
    for used in $used_opts; do
        ok=0

        for allowed in "$@"; do
            if [ "$used" = "$allowed" ]; then
                ok=1
            fi
        done

        if [ "$ok" -eq 0 ]; then
            pr_err "Option '%s' doesn't apply to '%s'" "$used" "$operation"
            exit 1
        fi
    done
}

no_more_args ()
{
    if [ -n "$arg1" ]; then
        pr_err "Unexpected argument '%s'" "$arg1"
        exit 1
    fi
}

#
# systemd
#

systemd_userdir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
systemd_service="$systemd_userdir/urnetwork.service"
systemd_update_service="$systemd_userdir/urnetwork-update.service"
systemd_update_timer="$systemd_userdir/urnetwork-update.timer"

# `su - user' and `sudo -iu user' shells often have the user's manager
# running but no XDG_RUNTIME_DIR, which makes every `systemctl --user' fail
if [ -z "$XDG_RUNTIME_DIR" ] && [ -S "/run/user/$(id -u)/bus" ]; then
    XDG_RUNTIME_DIR="/run/user/$(id -u)"
    export XDG_RUNTIME_DIR
fi

if command -v systemctl > /dev/null; then
    if systemctl --user show-environment > /dev/null 2>&1; then
        has_systemd=1
    else
        no_user_manager=1
    fi
fi

explain_no_user_manager ()
{
    pr_err "warning: systemd is installed, but there is no systemd user session for %s" "$(id -un)"
    pr_err "warning: This happens in \`sudo su' shells, some containers and WSL without systemd"
    pr_err "warning: Log in as %s directly (SSH or console) and run this again to get the background service" "$(id -un)"
}

require_systemd ()
{
    if [ "$has_systemd" -eq 1 ]; then
        return 0
    fi

    if [ "$no_user_manager" -eq 1 ]; then
        pr_err "No systemd user session is available for %s" "$(id -un)"
        pr_err "Log in as %s directly (SSH or console), then run this again" "$(id -un)"
    else
        pr_err "This system doesn't have systemd, so there is no background service to manage"
        pr_err "Run the provider in a terminal instead: urnetwork provide"
    fi

    exit 1
}

unit_enabled ()
{
    [ "$(systemctl --user is-enabled "$1" 2>/dev/null)" = "enabled" ]
}

unit_active ()
{
    systemctl --user is-active --quiet "$1" 2>/dev/null
}

signed_in ()
{
    [ -s "$state_dir/jwt" ]
}

# whether this device has its own provider identity yet. The miner only
# creates one when started with --allow-client-registration; without it,
# a new device's `provide' reports startup_recovery_required and exits 0.
device_registered ()
{
    # one .provider.jwt, or a .provider-<hash>.jwt per proxy in ~/.urnetwork/proxy
    found=0

    for jwt_file in "$state_dir/.provider.jwt" "$state_dir"/.provider-*.jwt; do
        if [ -s "$jwt_file" ]; then
            found=1
        fi
    done

    if [ "$found" -eq 0 ]; then
        return 1
    fi

    # a registration that was started but has no jwt yet
    for record in "$state_dir"/.provider*.jwt.registration; do
        if [ -e "$record" ] && [ ! -s "${record%.registration}" ]; then
            return 1
        fi
    done

    return 0
}

# First start of a new device: run the miner once in the foreground with
# --allow-client-registration until it has written its provider jwt, then stop
# it so the service takes over. The flag never replaces a retained identity.
reg_pid=""

stop_registering_miner ()
{
    if [ -z "$reg_pid" ]; then
        return 0
    fi

    kill "$reg_pid" 2>/dev/null
    waited=0

    while [ "$waited" -lt 15 ] && kill -0 "$reg_pid" 2>/dev/null; do
        sleep 1
        waited=$((waited + 1))
    done

    kill -9 "$reg_pid" 2>/dev/null
    wait "$reg_pid" 2>/dev/null
    reg_pid=""
}

register_device ()
{
    if device_registered; then
        return 0
    fi

    pr_info "Registering this device with URnetwork (first start, up to 2 minutes)"

    if ! reg_log="$(mktemp "${TMPDIR:-/tmp}/urnetwork-register.XXXXXX")"; then
        reg_log=/dev/null
    fi

    # never leave it running with --allow-client-registration: it would hold
    # the registration lock and keep the service from starting
    trap 'stop_registering_miner; [ "$reg_log" != /dev/null ] && rm -f "$reg_log"; cleanup' EXIT
    trap 'exit 1' INT TERM HUP

    # 9>&-: not the install lock
    "$install_path/bin/urnetwork" provide --allow-client-registration < /dev/null > "$reg_log" 2>&1 9>&- &
    reg_pid=$!
    waited=0

    while [ "$waited" -lt 120 ] && ! device_registered && kill -0 "$reg_pid" 2>/dev/null; do
        sleep 1
        waited=$((waited + 1))
    done

    exited_early=0

    if ! device_registered && ! kill -0 "$reg_pid" 2>/dev/null; then
        exited_early=1
    fi

    stop_registering_miner
    trap cleanup EXIT

    if device_registered; then
        [ "$reg_log" != /dev/null ] && rm -f "$reg_log"
        pr_info "This device is registered"
        return 0
    fi

    if [ "$exited_early" -eq 1 ]; then
        pr_err "Registering this device failed. The miner said:"
    else
        pr_err "Registering this device didn't finish within 2 minutes. The miner said:"
    fi

    reg_hint=""

    if [ "$reg_log" != /dev/null ]; then
        tail -n 8 "$reg_log" | sed 's/^/    /' >&2

        if grep -q -i -E 'network_sign_in|401|unauthori' "$reg_log"; then
            reg_hint="sign_in"
        fi

        rm -f "$reg_log"
    fi

    if [ "$reg_hint" = "sign_in" ]; then
        pr_err "Your sign-in was refused. Sign in again with \`urnetwork auth' (code at https://ur.io), then run \`urnet-tools start'"
    elif [ "$exited_early" -eq 1 ]; then
        pr_err "See https://ur.xyz/docs/miner for what this means, then run \`urnet-tools start' again"
    else
        pr_err "Check your internet connection and run \`urnet-tools start' again"
    fi

    return 1
}

# an enabled service on a device that never registered exits at once without a
# word (and isn't restarted), so updates finish the registration for it
repair_unregistered_service ()
{
    if [ "$has_systemd" -eq 1 ] && signed_in && unit_enabled urnetwork.service &&
        ! unit_active urnetwork.service && ! device_registered; then
        pr_info "urnetwork.service is on but this device was never registered; registering it now"
        start_provider || pr_err "warning: The provider isn't running; run \`urnet-tools start'"
    fi
}

# prints the configured update interval, read back from the timer unit
current_interval ()
{
    if [ ! -f "$systemd_update_timer" ]; then
        return 0
    fi

    case "$(sed -n 's/^OnCalendar=//p' "$systemd_update_timer" | head -n1)" in
        *00/6:00:00) echo 6h ;;
        daily) echo daily ;;
        weekly) echo weekly ;;
        monthly) echo monthly ;;
    esac
}

interval_label ()
{
    case "$1" in
        6h) echo "every 6 hours" ;;
        "") echo "unknown interval" ;;
        *) echo "$1" ;;
    esac
}

interval_oncalendar ()
{
    case "$1" in
        6h) echo "*-*-* 00/6:00:00" ;;
        *) echo "$1" ;;
    esac
}

# systemd expands %-specifiers inside unit values
unit_path ()
{
    printf '%s' "$1" | sed 's/%/%%/g'
}

# $1: interval
write_systemd_units ()
{
    if ! mkdir -p "$systemd_userdir"; then
        pr_err "Failed to create directory '%s'" "$systemd_userdir"
        exit 1
    fi

    pr_info "Installing urnetwork.service in %s" "$systemd_service"

    cat > "$systemd_service" <<EOF
[Unit]
Description=URnetwork Provider
Documentation=https://ur.xyz/docs/miner

[Service]
ExecStart="$(unit_path "$install_path")/bin/urnetwork" provide
Restart=on-failure
RestartSec=30

[Install]
WantedBy=default.target
EOF

    pr_info "Installing urnetwork-update.service in %s" "$systemd_update_service"

    cat > "$systemd_update_service" <<EOF
[Unit]
Description=URnetwork Update

[Service]
Type=oneshot
ExecStart="$(unit_path "$install_path")/bin/urnet-tools" update
EOF

    pr_info "Installing urnetwork-update.timer in %s" "$systemd_update_timer"

    cat > "$systemd_update_timer" <<EOF
[Unit]
Description=Run URnetwork Update

[Timer]
OnCalendar=$(interval_oncalendar "$1")
RandomizedDelaySec=1h
Persistent=true

[Install]
WantedBy=default.target
EOF

    if ! systemctl --user daemon-reload; then
        pr_err "Failed to reload the systemd user manager"
        exit 1
    fi
}

#
# install, update, reinstall
#

workdir=""
service_stopped=0

# one install/update at a time (the timer, a person, or the miner's own
# `provider update' can all start one)
take_lock ()
{
    if ! command -v flock > /dev/null; then
        return 0
    fi

    lock_file="${XDG_RUNTIME_DIR:-$cache_dir}/urnetwork-install.lock"

    # a failed redirect on `exec' ends dash, so try it in a subshell first
    if ! (: >> "$lock_file") 2>/dev/null; then
        return 0
    fi

    exec 9>> "$lock_file"

    if ! flock -n 9; then
        pr_err "Another URnetwork install or update is running; try again in a few minutes"
        exit 1
    fi
}

# whether version $1 sorts after $2 (needs `sort -V'; without it, never)
version_newer ()
{
    if [ "$1" = "$2" ] || ! printf '1\n2\n' | sort -V > /dev/null 2>&1; then
        return 1
    fi

    [ "$(printf '%s\n%s\n' "$1" "$2" | sed 's/^v//' | sort -V | tail -n1)" = "$(echo "$1" | sed 's/^v//')" ]
}

cleanup ()
{
    if [ -n "$workdir" ] && [ -d "$workdir" ]; then
        rm -rf "$workdir"
    fi

    # a failed update must not leave the miner stopped
    if [ "$service_stopped" -eq 1 ]; then
        systemctl --user start urnetwork.service > /dev/null 2>&1
    fi
}

# $1: script file; prints its tools_version, 0 if it has none (older scripts)
script_tools_version ()
{
    v="$(sed -n 's/^tools_version=\([0-9][0-9]*\)$/\1/p' "$1" | head -n1)"
    echo "${v:-0}"
}

# $1: script file; whether it looks like a whole urnet-tools
valid_tools_script ()
{
    [ "$(head -n1 "$1")" = "#!/bin/sh" ] &&
        grep -q "^# urnet-tools -- URnetwork manager script" "$1" &&
        sh -n "$1" 2>/dev/null
}

# Picks the urnet-tools to install into $workdir/tools.sh before anything is
# changed. An update takes the newest one from $script_url, so installer fixes
# reach existing installs; `curl | sh' has no file of its own and must fetch too.
prepare_tools_script ()
{
    tools_file="$workdir/tools.sh"
    fetched_file="$workdir/tools.fetched"
    self_file=""

    if [ -f "$0" ] && grep -q "^# urnet-tools -- URnetwork manager script" "$0" 2>/dev/null; then
        self_file="$0"
    fi

    if [ "$operation" != "install" ] || [ -z "$self_file" ]; then
        if network_fetch "$script_url" > "$fetched_file" 2>/dev/null && valid_tools_script "$fetched_file"; then
            if [ "$(script_tools_version "$fetched_file")" -ge "$tools_version" ]; then
                cp "$fetched_file" "$tools_file"
                return 0
            fi

            fetch_problem="is older than this one"
        else
            fetch_problem="could not be downloaded"
        fi

        if [ -z "$self_file" ]; then
            pr_err "Could not get urnet-tools: the one at %s %s" "$script_url" "$fetch_problem"
            pr_err "Installing from a fork or branch? Pass the same URL to sh:"
            pr_err "  curl -fsSL <url> | URNETWORK_TOOLS_URL=<url> sh"
            pr_err "Nothing was changed"
            exit 1
        fi

        if [ "$operation" != "install" ]; then
            pr_err "warning: Keeping this urnet-tools; the one at %s %s" "$script_url" "$fetch_problem"
        fi
    fi

    cp "$self_file" "$tools_file"
}

install_tools_script ()
{
    if ! tools_tmp="$(mktemp "$install_path/bin/.urnet-tools.XXXXXX")"; then
        pr_err "Failed to install urnet-tools"
        exit 1
    fi

    {
        head -n1 "$tools_file"
        echo "URNETWORK_TOOLS_MODE=1"
        echo "URNETWORK_INSTALL_PATH=$(sh_quote "$install_path")"
        echo "URNETWORK_TOOLS_URL=$(sh_quote "$script_url")"
        awk 'NR > 1 && !(NR <= 6 && /^URNETWORK_(TOOLS_MODE|INSTALL_PATH|TOOLS_URL|TOOLS_SCRIPT_URL)=/)' "$tools_file"
    } > "$tools_tmp" || { rm -f "$tools_tmp"; pr_err "Failed to install urnet-tools"; exit 1; }

    chmod 755 "$tools_tmp" || { rm -f "$tools_tmp"; pr_err "Failed to install urnet-tools"; exit 1; }

    # a new file, not an overwrite: the running urnet-tools keeps reading the old one
    mv -f "$tools_tmp" "$install_path/bin/urnet-tools" || { rm -f "$tools_tmp"; pr_err "Failed to install urnet-tools"; exit 1; }
}

# $1: rc file; adds the PATH block once
add_path_block ()
{
    if [ -f "$1" ] && grep -q '^[[:space:]]*# == urnetwork-provider start' "$1"; then
        if grep -qxF "export URNETWORK_PROVIDER_INSTALL=$(sh_quote "$install_path")" "$1" ||
            grep -qxF "export URNETWORK_PROVIDER_INSTALL=\"$install_path\"" "$1"; then
            pr_info "%s is up-to-date" "$1"
            return 0
        fi

        # it points at another install path
        remove_path_block "$1"
    fi

    pr_info "Adding '%s' to PATH in %s" "$install_path/bin" "$1"

    if ! cat >> "$1" <<EOF

# == urnetwork-provider start
export URNETWORK_PROVIDER_INSTALL=$(sh_quote "$install_path")
export PATH="\$PATH:\$URNETWORK_PROVIDER_INSTALL/bin"
# == urnetwork-provider end
EOF
    then
        pr_err "warning: Could not write %s; add %s to your PATH yourself" "$1" "$install_path/bin"
    fi
}

# $1: rc file; removes the PATH block
remove_path_block ()
{
    if [ ! -f "$1" ] || ! grep -q '# == urnetwork-provider start' "$1"; then
        return 0
    fi

    pr_info "Removing PATH exports from %s" "$1"
    cp "$1" "$1.backup.old"
    awk '/# == urnetwork-provider start/ { pr=1 } pr == 0 { print } /# == urnetwork-provider end/ { pr=0 }' "$1" > "$1.new" &&
        cat "$1.new" > "$1"
    rm -f "$1.new"
}

# $1: interval for a new timer. Rewrites the units and puts them back in the
# state the person left them in; a new install enables auto-update.
setup_systemd ()
{
    new_install=0

    if [ ! -f "$systemd_service" ]; then
        new_install=1
    fi

    service_was_enabled=0
    timer_was_enabled=0

    if unit_enabled urnetwork.service; then
        service_was_enabled=1
    fi

    if unit_enabled urnetwork-update.timer; then
        timer_was_enabled=1
    fi

    timer_interval="$interval"

    if [ -z "$timer_interval" ]; then
        timer_interval="$(current_interval)"
    fi

    if [ -z "$timer_interval" ]; then
        timer_interval="$1"
    fi

    write_systemd_units "$timer_interval"

    if [ "$new_install" -eq 1 ] || [ "$timer_was_enabled" -eq 1 ]; then
        if ! systemctl --user enable --now urnetwork-update.timer; then
            pr_err "warning: Could not turn on automatic updates; try \`urnet-tools auto-update on'"
        fi
    else
        pr_info "Automatic updates stay off (turn them on with \`urnet-tools auto-update on')"
    fi

    # the provider is only enabled once it is signed in, or it would fail at every boot
    if [ "$service_was_enabled" -eq 1 ] && ! systemctl --user enable urnetwork.service; then
        pr_err "warning: Could not re-enable urnetwork.service"
    fi
}

# whether a person can be asked; under `curl | sh' stdin is the script, so ask /dev/tty
have_tty ()
{
    (exec < /dev/tty) 2>/dev/null
}

sign_in ()
{
    if signed_in; then
        return 0
    fi

    if [ -z "$auth_code" ] && [ "$no_start" -eq 0 ] && [ "$assume_yes" -eq 0 ] && have_tty; then
        printf "\n"
        printf "Sign in to start mining. Get an auth code at %shttps://ur.io%s\n" "$bold" "$plain"
        printf "Auth code (or press Enter to do this later): "
        read -r auth_code < /dev/tty || auth_code=""
    fi

    if [ -z "$auth_code" ]; then
        return 1
    fi

    if "$install_path/bin/urnetwork" auth "$auth_code"; then
        auth_code=""
        return 0
    fi

    auth_code=""
    pr_err "Signing in failed. Check the code at https://ur.io and run: urnetwork auth"
    return 1
}

# starts and enables the provider service; $1 = force starts it even when
# ~/.urnetwork/jwt is missing (other sign-in kinds)
start_provider ()
{
    if ! signed_in; then
        if [ "$1" != "force" ]; then
            pr_err "Not signed in yet. Run \`urnetwork auth' first (get a code at https://ur.io)"
            return 1
        fi

        pr_err "warning: %s has no sign-in; if the provider stops, run \`urnetwork auth' (code at https://ur.io)" "$state_dir"
    elif ! unit_active urnetwork.service && ! register_device; then
        return 1
    fi

    if ! systemctl --user enable urnetwork.service > /dev/null 2>&1; then
        pr_err "warning: Could not enable urnetwork.service to start at boot/login"
    fi

    if unit_active urnetwork.service; then
        return 0
    fi

    pr_info "Starting urnetwork.service"

    if ! systemctl --user start urnetwork.service; then
        pr_err "Failed to start urnetwork.service. See: journalctl --user -u urnetwork"
        return 1
    fi

    sleep 2

    if ! unit_active urnetwork.service; then
        pr_err "urnetwork.service stopped right after starting. See: journalctl --user -u urnetwork"
        return 1
    fi

    return 0
}

# Without lingering the user's systemd manager, and urnetwork.service with it,
# stops at logout and only starts again at the next login.
enable_lingering ()
{
    if [ "$has_systemd" -ne 1 ]; then
        return 0
    fi

    linger_user="$(id -un)"

    if [ "$(loginctl show-user "$linger_user" --property=Linger --value 2>/dev/null)" = "yes" ]; then
        return 0
    fi

    if loginctl enable-linger "$linger_user" &&
        [ "$(loginctl show-user "$linger_user" --property=Linger --value 2>/dev/null)" = "yes" ]; then
        pr_info "Enabled lingering so urnetwork.service keeps running after logout"
        return 0
    fi

    pr_err "warning: Could not enable lingering for %s; urnetwork.service will stop when you log out" "$linger_user"
    pr_err "warning: Run \`sudo loginctl enable-linger %s' to keep the provider running" "$linger_user"
}

do_install ()
{
    case "$operation" in
        install)
            allow_opts --tag --no-modify-bashrc --interval --auth-code --no-start --yes
            ;;

        update)
            allow_opts
            no_modify_bashrc=1
            ;;

        reinstall)
            allow_opts --tag --no-modify-bashrc

            if [ -z "$tag" ]; then
                tag="$(installed_version)"
            fi

            if [ -z "$tag" ]; then
                pr_err "Could not determine the currently installed version; use --tag TAG or --tag latest"
                exit 1
            fi
            ;;
    esac

    no_more_args

    if [ -n "$tag" ] && [ "$tag" != "latest" ] && [ "$(echo "$tag" | cut -c -1)" != "v" ]; then
        tag="v$tag"
    fi

    if [ -z "$tag" ]; then
        tag="latest"
    fi

    if [ "$operation" != "install" ] && [ ! -d "$install_path/bin" ]; then
        pr_err "URnetwork is not installed in '%s'" "$install_path"
        exit 1
    fi

    arch="$(get_arch)"

    if [ -z "$arch" ]; then
        pr_err "This installer has no URnetwork build for '%s' processors" "$(uname -m)"
        pr_err "Supported: x86_64, i686, aarch64 (64-bit ARM), armv7l (32-bit ARM)"
        pr_err "Other builds (e.g. MIPS routers) are in the release: https://github.com/urnetwork/build/releases/latest"
        exit 1
    fi

    if [ "$no_user_manager" -eq 1 ]; then
        explain_no_user_manager
    fi

    require_json_tool

    # on disk next to the install: the all-platform download is ~240 MB and
    # /tmp is often a small tmpfs
    cache_dir="${XDG_CACHE_HOME:-$HOME/.cache}"

    if ! { mkdir -p "$cache_dir" && workdir="$(mktemp -d "$cache_dir/urnetwork-install.XXXXXX")"; } &&
        ! workdir="$(mktemp -d)"; then
        pr_err "Failed to create working directory"
        exit 1
    fi

    trap cleanup EXIT
    trap 'exit 1' INT TERM HUP

    take_lock
    prepare_tools_script

    if [ "$tag" = "latest" ]; then
        api_url="$api_base/releases/latest"
    else
        api_url="$api_base/releases/tags/$tag"
    fi

    pr_info "Fetching release information for tag: %s" "$tag"

    release_json="$workdir/release.json"

    if ! network_fetch "$api_url" > "$release_json"; then
        pr_err "Failed to fetch release information for tag: %s" "$tag"
        pr_err "Check your internet connection, or that the tag exists: https://github.com/urnetwork/build/releases"
        exit 1
    fi

    version_to_install="$(release_tag "$release_json")"

    if [ -z "$version_to_install" ]; then
        pr_err "Could not read the release information for tag: %s" "$tag"
        exit 1
    fi

    if [ "$operation" = "update" ]; then
        current="$(installed_version)"

        if [ "$current" = "$version_to_install" ] || version_newer "$current" "$version_to_install"; then
            if [ "$current" = "$version_to_install" ]; then
                pr_info "Installed version %s is up-to-date" "$current"
            else
                pr_info "Installed version %s is newer than the latest release %s; keeping it" "$current" "$version_to_install"
            fi

            # still pick up urnet-tools fixes
            install_tools_script

            # an install made by the old script left its timer stopped after an update
            if [ "$has_systemd" -eq 1 ] && unit_enabled urnetwork-update.timer &&
                ! unit_active urnetwork-update.timer; then
                systemctl --user start urnetwork-update.timer
            fi

            repair_unregistered_service
            exit 0
        fi

        pr_info "Updating %s to %s" "${current:-unknown}" "$version_to_install"
    fi

    # a per-platform file when the release has one, else the all-platform one
    dl_name=""
    dl_url=""
    dl_digest=""
    all_name=""
    all_url=""
    all_digest=""
    tab="$(printf '\t')"

    assets="$(release_assets "$release_json")"

    while IFS="$tab" read -r a_name a_url a_digest; do
        case "$a_name" in
            urnetwork-provider-*-linux-"$arch".tar.gz)
                dl_name="$a_name"
                dl_url="$a_url"
                dl_digest="$a_digest"
                ;;

            urnetwork-provider-*-linux-*|urnetwork-provider-*-darwin-*|urnetwork-provider-*-windows-*)
                ;;

            urnetwork-provider-*.tar.gz)
                all_name="$a_name"
                all_url="$a_url"
                all_digest="$a_digest"
                ;;
        esac
    done <<EOF
$assets
EOF

    if [ -z "$dl_url" ]; then
        dl_name="$all_name"
        dl_url="$all_url"
        dl_digest="$all_digest"
    fi

    if [ -z "$dl_url" ]; then
        pr_err "The release %s has no URnetwork provider download" "$version_to_install"
        exit 1
    fi

    expected_sha256=""

    case "$dl_digest" in
        sha256:*)
            expected_sha256="${dl_digest#sha256:}"
            ;;
    esac

    if [ -z "$expected_sha256" ]; then
        expected_sha256="$(release_body_sha256 "$release_json" "$dl_name")"
    fi

    pr_info "Downloading: %s" "$dl_url"

    tarball="$workdir/$dl_name"

    if command -v curl > /dev/null; then
        progress="-sS"

        if [ -t 2 ]; then
            progress="--progress-bar"
        fi

        if ! curl -fL "$progress" "$dl_url" -o "$tarball"; then
            pr_err "Failed to download %s" "$dl_url"
            exit 1
        fi
    elif command -v wget > /dev/null; then
        if ! wget -q -O "$tarball" "$dl_url"; then
            pr_err "Failed to download %s" "$dl_url"
            exit 1
        fi
    else
        pr_err "Neither curl nor wget is available"
        exit 1
    fi

    if [ -n "$expected_sha256" ]; then
        actual_sha256="$(file_sha256 "$tarball")"

        if [ -z "$actual_sha256" ]; then
            pr_err "warning: No sha256sum, shasum or openssl here; skipping the checksum check"
        elif [ "$(echo "$actual_sha256" | tr 'A-F' 'a-f')" != "$(echo "$expected_sha256" | tr 'A-F' 'a-f')" ]; then
            pr_err "Checksum mismatch for %s" "$dl_name"
            pr_err "  expected %s" "$expected_sha256"
            pr_err "  got      %s" "$actual_sha256"
            pr_err "The download is damaged or was changed. Nothing was installed; try again later"
            exit 1
        else
            pr_info "Checksum OK (sha256 %s)" "$actual_sha256"
        fi
    elif [ "$operation" = "update" ]; then
        pr_err "The release lists no checksum for %s; not updating without one" "$dl_name"
        pr_err "Run \`urnet-tools reinstall --tag %s' to install it anyway" "$version_to_install"
        exit 1
    else
        pr_err "warning: The release lists no checksum for %s; installing without a checksum check" "$dl_name"
    fi

    # only this machine's binary, not all twelve platforms
    extract_dir="$workdir/x"
    mkdir -p "$extract_dir"
    bin_program=""

    for member in "linux/$arch/provider" "./linux/$arch/provider" "provider" "./provider"; do
        if tar -xzf "$tarball" -C "$extract_dir" "$member" 2>/dev/null && [ -f "$extract_dir/$member" ]; then
            bin_program="$extract_dir/$member"
            break
        fi
    done

    if [ -z "$bin_program" ]; then
        pr_err "The download has no provider program for linux/%s" "$arch"
        pr_err "This indicates an issue with the release; nothing was installed"
        exit 1
    fi

    if [ "$(head -c 4 "$bin_program" | od -An -c | tr -d ' ')" != '177ELF' ]; then
        pr_err "The provider program in the download isn't a Linux program; nothing was installed"
        exit 1
    fi

    # 126/127: the kernel can't run it (wrong architecture, noexec, missing loader)
    chmod 755 "$bin_program"
    "$bin_program" --version > /dev/null 2>&1 < /dev/null
    run_status=$?

    if [ "$run_status" -eq 126 ] || [ "$run_status" -eq 127 ]; then
        pr_err "The downloaded provider program can't run on this machine; nothing was installed"
        exit 1
    fi

    if [ -d "$install_path/bin" ] && [ "$operation" = "install" ]; then
        pr_info "Found an existing installation in %s, updating it" "$install_path"
    fi

    if ! mkdir -p "$install_path/bin"; then
        pr_err "Failed to create directory '%s'" "$install_path/bin"
        exit 1
    fi

    if [ "$has_systemd" -eq 1 ] && unit_active urnetwork.service; then
        pr_info "Stopping urnetwork.service for the update; it starts again afterwards"
        systemctl --user stop urnetwork.service
        service_stopped=1
    fi

    # replace, never overwrite: a provider running in a terminal keeps its old file
    if ! { bin_tmp="$(mktemp "$install_path/bin/.urnetwork.XXXXXX")" &&
        cp "$bin_program" "$bin_tmp" &&
        chmod 755 "$bin_tmp" &&
        mv -f "$bin_tmp" "$install_path/bin/urnetwork"; }; then
        rm -f "$bin_tmp"
        pr_err "Failed to install provider binary"
        exit 1
    fi

    install_tools_script

    echo "$version_to_install" > "$install_path/.version"
    rm -f "$install_path/.date"

    if [ "$has_systemd" -eq 1 ]; then
        setup_systemd "$default_interval"
    fi

    if [ "$service_stopped" -eq 1 ]; then
        service_stopped=0

        if ! systemctl --user start urnetwork.service; then
            pr_err "warning: Unable to restart urnetwork.service after the update; run \`urnet-tools start'"
        fi
    fi

    if [ "$operation" != "install" ]; then
        repair_unregistered_service
    fi

    if [ "$no_modify_bashrc" -eq 0 ]; then
        add_path_block "$HOME/.bashrc"

        if [ -f "$HOME/.zshrc" ]; then
            add_path_block "$HOME/.zshrc"
        fi
    fi

    enable_lingering

    case "$operation" in
        install)
            install_summary
            ;;

        reinstall)
            pr_info "Reinstalled %s" "$version_to_install"
            ;;

        update)
            pr_info "Updated successfully to %s" "$version_to_install"
            ;;
    esac
}

install_summary ()
{
    running=0

    if [ "$no_start" -eq 0 ] && sign_in && [ "$has_systemd" -eq 1 ] && start_provider; then
        running=1
    fi

    pr_info "Installed URnetwork %s in %s" "$version_to_install" "$install_path"
    printf "\n"

    if [ "$running" -eq 1 ]; then
        if [ "$(loginctl show-user "$(id -un)" --property=Linger --value 2>/dev/null)" = "yes" ]; then
            printf "%sThe provider is running%s in the background and starts again after a reboot.\n" "$bold" "$plain"
        else
            printf "%sThe provider is running%s in the background until you log out (see the lingering warning above).\n" "$bold" "$plain"
        fi
        printf "Automatic updates: %s (urnet-tools auto-update)\n\n" "$(unit_enabled urnetwork-update.timer && echo "on, $(interval_label "$(current_interval)")" || echo off)"
    fi

    case ":$PATH:" in
        *":$install_path/bin:"*)
            ;;

        *)
            if [ "$no_modify_bashrc" -eq 0 ]; then
                printf "Open a new terminal, or run:  %sexport PATH=\"\$PATH:%s/bin\"%s\n" "$bold" "$install_path" "$plain"
            else
                printf "Add %s to your PATH to use the commands below.\n" "$install_path/bin"
            fi
            ;;
    esac

    if [ "$has_systemd" -eq 1 ]; then
        if [ "$running" -eq 0 ]; then
            if ! signed_in; then
                printf "Sign in:         %surnetwork auth%s              # auth code from <https://ur.io>\n" "$bold" "$plain"
            fi

            printf "Start:           %surnet-tools start%s           # runs in the background, also after reboot\n" "$bold" "$plain"
        fi

        printf "Check on it:     %surnet-tools status%s\n" "$bold" "$plain"
        printf "Live log:        %surnet-tools logs%s\n" "$bold" "$plain"
        printf "Stop:            %surnet-tools stop%s\n" "$bold" "$plain"
    else
        if ! signed_in; then
            printf "Sign in:         %surnetwork auth%s              # auth code from <https://ur.io>\n" "$bold" "$plain"
        fi

        if device_registered; then
            printf "Start:           %surnetwork provide%s           # in the foreground; keep the terminal open\n" "$bold" "$plain"
        else
            printf "First start:     %surnetwork provide --allow-client-registration%s   # registers this device; keep the terminal open\n" "$bold" "$plain"
            printf "After that:      %surnetwork provide%s\n" "$bold" "$plain"
        fi
        printf "No background service or automatic updates without a systemd user session.\n"
        printf "Update later with:  %surnet-tools update%s\n" "$bold" "$plain"
    fi

    printf "\n"
    printf "%sRefer to <https://ur.xyz/docs/miner> for more detailed instructions.%s\n" "$bold" "$plain"
}

#
# uninstall
#

confirm_purge ()
{
    if [ "$assume_yes" -eq 1 ]; then
        return 0
    fi

    if ! have_tty; then
        pr_err "Not deleting %s without confirmation; add --yes to delete it" "$state_dir"
        return 1
    fi

    printf "Delete %s? It holds this device's miner identity and settings, and\n" "$state_dir" > /dev/tty
    printf "can't be recovered. Type 'delete' to confirm: " > /dev/tty
    read -r answer < /dev/tty || answer=""
    [ "$answer" = "delete" ]
}

do_uninstall ()
{
    allow_opts --no-modify-bashrc --remove-identity --yes
    no_more_args

    # `rm -r' below: only ever on something that looks like an install
    if [ -d "$install_path" ]; then
        case "$install_path" in
            /|"$HOME"|"$HOME/"|"$state_dir"|"$state_dir/")
                pr_err "Refusing to remove '%s'" "$install_path"
                exit 1
                ;;
        esac

        if [ ! -f "$install_path/.version" ] && [ ! -f "$install_path/bin/urnetwork" ]; then
            pr_err "'%s' doesn't look like a URnetwork installation; not removing it" "$install_path"
            exit 1
        fi
    fi

    # ask before removing anything, so a "no" leaves everything as it was
    delete_state=0

    if [ "$purge" -eq 1 ] && [ -d "$state_dir" ]; then
        if ! confirm_purge; then
            pr_err "Nothing was uninstalled"
            exit 1
        fi

        delete_state=1
    fi

    found=0

    if [ -f "$systemd_service" ] || [ -f "$systemd_update_timer" ] || [ -f "$systemd_update_service" ]; then
        found=1

        if [ "$no_user_manager" -eq 1 ]; then
            pr_err "There is no systemd user session for %s, so the miner can't be stopped from here" "$(id -un)"
            pr_err "Log in as %s directly (SSH or console) and run this again. Nothing was removed" "$(id -un)"
            exit 1
        fi

        if [ "$has_systemd" -eq 1 ]; then
            pr_info "Stopping and removing the systemd units"

            # one by one: disabling a unit whose file is gone fails
            for unit in urnetwork.service urnetwork-update.timer; do
                systemctl --user disable --now "$unit" > /dev/null 2>&1
            done

            systemctl --user stop urnetwork-update.service > /dev/null 2>&1

            if unit_active urnetwork.service; then
                pr_err "Could not stop urnetwork.service; nothing else was removed"
                exit 1
            fi
        fi

        rm -f "$systemd_service" "$systemd_update_service" "$systemd_update_timer"

        if [ "$has_systemd" -eq 1 ]; then
            systemctl --user daemon-reload
            systemctl --user reset-failed urnetwork.service urnetwork-update.service > /dev/null 2>&1
        fi
    fi

    if [ -d "$install_path" ]; then
        found=1
        pr_info "Removing: %s" "$install_path"

        if ! rm -r "$install_path"; then
            pr_err "Failed to completely remove '%s'" "$install_path"
            exit 1
        fi
    fi

    if [ "$found" -eq 0 ]; then
        pr_err "Directory '%s' could not be found, are you sure you have URnetwork installed?" "$install_path"
        exit 1
    fi

    if [ "$no_modify_bashrc" -eq 0 ]; then
        remove_path_block "$HOME/.bashrc"
        remove_path_block "$HOME/.zshrc"
    fi

    if [ -d "$state_dir" ]; then
        if [ "$delete_state" -eq 1 ]; then
            pr_info "Removing: %s" "$state_dir"
            rm -rf "$state_dir"
        else
            pr_info "Kept your miner identity in %s; installing again picks it up" "$state_dir"
            pr_info "To delete it as well: rm -rf %s  (it can't be recovered)" "$state_dir"
        fi
    fi

    pr_info "Uninstallation successful"
}

#
# auto-update, auto-start, start, stop, status
#

change_auto_update_prefs ()
{
    allow_opts --interval
    mode="$arg1"

    if [ -n "$mode" ] && [ "$mode" != "on" ] && [ "$mode" != "off" ]; then
        pr_err "Invalid argument '%s': Must be either 'on' or 'off'" "$mode"
        exit 1
    fi

    require_systemd

    if [ -z "$mode" ] && [ -n "$interval" ]; then
        if [ ! -f "$systemd_service" ]; then
            pr_err "URnetwork's systemd units are missing; run \`%s reinstall' first" "$me"
            exit 1
        fi

        write_systemd_units "$interval" > /dev/null

        if unit_enabled urnetwork-update.timer; then
            systemctl --user restart urnetwork-update.timer
            pr_info "Automatic updates: on, %s" "$(interval_label "$interval")"
        else
            pr_info "Interval set to %s; automatic updates stay off (\`%s auto-update on' turns them on)" "$(interval_label "$interval")" "$me"
        fi

        exit 0
    fi

    if [ -z "$mode" ]; then
        state="$(systemctl --user is-enabled urnetwork-update.timer 2>/dev/null)"
        pr_info "Auto update state: %s" "${state:-not installed}"

        if [ -n "$(current_interval)" ]; then
            pr_info "Interval: %s" "$(current_interval)"
        fi

        exit 0
    fi

    case "$mode" in
        on)
            new_interval="${interval:-$(current_interval)}"
            new_interval="${new_interval:-$default_interval}"

            pr_info "Updating systemd unit files (interval: %s)" "$new_interval"

            if [ ! -f "$systemd_service" ]; then
                pr_err "URnetwork's systemd units are missing; run \`%s reinstall' first" "$me"
                exit 1
            fi

            write_systemd_units "$new_interval"

            if ! systemctl --user enable --now urnetwork-update.timer; then
                pr_err "Failed to turn on auto updates: systemctl command failed"
                exit 1
            fi

            pr_info "Automatic updates are on, %s" "$(interval_label "$new_interval")"
            ;;

        off)
            if ! systemctl --user disable --now urnetwork-update.timer; then
                pr_err "Failed to turn off auto updates: systemctl command failed"
                exit 1
            fi

            pr_info "Automatic updates are off"
            ;;
    esac
}

toggle_auto_start ()
{
    allow_opts
    require_systemd

    if test -z "$arg1"; then
        pr_err "Must provide an argument: Either 'on' or 'off'"
        exit 1
    fi

    if test "$arg1" != on && test "$arg1" != off; then
        pr_err "Invalid value: %s, must be either on or off" "$arg1"
        exit 1
    fi

    if test "$arg1" = on; then
        if unit_enabled urnetwork.service; then
            pr_info "urnetwork.service already starts at boot/login"
        else
            pr_info "Enabling urnetwork.service (at boot/login)"
            systemctl --user enable urnetwork.service || exit 1
        fi
    else
        if ! unit_enabled urnetwork.service; then
            pr_info "urnetwork.service is already disabled"
        else
            pr_info "Disabling urnetwork.service"
            systemctl --user disable urnetwork.service || exit 1
        fi
    fi
}

do_start ()
{
    allow_opts
    no_more_args
    require_systemd

    if unit_active urnetwork.service; then
        pr_info "Service urnetwork.service is already active"
        return 0
    fi

    start_provider force || exit 1
    pr_info "The provider is running. Check on it with: urnet-tools status"
}

do_stop ()
{
    allow_opts
    no_more_args
    require_systemd

    if unit_active urnetwork.service; then
        pr_info "Stopping urnetwork.service"
        systemctl --user stop urnetwork.service || { pr_err "Failed to stop urnetwork.service"; exit 1; }
    else
        pr_info "Service urnetwork.service is not active"
    fi
}

show_status ()
{
    allow_opts
    no_more_args

    yes_no ()
    {
        if "$@"; then echo yes; else echo no; fi
    }

    echo "Installed:     $(installed_version) in $install_path"
    echo "Signed in:     $(yes_no signed_in)"
    echo "Registered:    $(yes_no device_registered)"

    if [ "$has_systemd" -ne 1 ]; then
        echo "Service:       none (no systemd user session)"
        exit 3
    fi

    echo "Running:       $(systemctl --user is-active urnetwork.service 2>/dev/null)"
    echo "Start at boot: $(yes_no unit_enabled urnetwork.service)"

    if unit_enabled urnetwork-update.timer; then
        echo "Auto-update:   on, $(interval_label "$(current_interval)")"
    else
        echo "Auto-update:   off"
    fi

    if [ "$(loginctl show-user "$(id -un)" --property=Linger --value 2>/dev/null)" != "yes" ]; then
        echo "Note:          lingering is off, so the provider stops when you log out"
    fi

    echo ""
    systemctl --user status urnetwork.service --no-pager -n 5 2>/dev/null

    if unit_active urnetwork.service; then
        return 0
    fi

    return 3
}

show_logs ()
{
    allow_opts
    no_more_args
    require_systemd
    exec journalctl --user -u urnetwork.service -f
}

case "$operation" in
    install|update|reinstall)
        do_install
        exit 0
        ;;

    uninstall)
        do_uninstall
        exit 0
        ;;

    auto-update)
        change_auto_update_prefs
        exit 0
        ;;

    auto-start)
        toggle_auto_start
        exit 0
        ;;

    start)
        do_start
        exit 0
        ;;

    stop)
        do_stop
        exit 0
        ;;

    status)
        show_status
        exit $?
        ;;

    restart)
        allow_opts
        no_more_args
        require_systemd
        systemctl --user stop urnetwork.service
        do_start
        exit 0
        ;;

    version)
        allow_opts
        no_more_args
        show_version
        exit 0
        ;;

    logs)
        show_logs
        ;;

    *)
        pr_err "Invalid operation '%s'" "$operation"
        pr_err "Try '%s --help' for more information" "$me"
        exit 1
        ;;
esac
