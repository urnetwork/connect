#!/bin/sh
# urnet-tools for macOS -- installs and manages the URnetwork provider
# GitHub: <https://github.com/urnetwork/connect>
#
# Install:  curl -fsSL https://raw.githubusercontent.com/urnetwork/connect/main/scripts/Provider_Install_macOS.sh | sh
#
# The same file is installed as urnet-tools. It runs the provider as a
# launchd LaunchAgent and keeps it up to date with a second LaunchAgent.
# Supports Apple silicon (arm64) and Intel (amd64) Macs.
#
# Piped through sh, it gets the urnet-tools copy to install from
# URNETWORK_TOOLS_URL (default: this file on urnetwork/connect main). To
# install from a branch or a fork, set it to that branch's raw URL, or
# download this file and run it with sh.

me="$0"
script_rundir="$(pwd)"

case "$me" in
    sh|bash|zsh|-sh|-bash|-zsh|/bin/sh|/bin/bash|/bin/zsh) me="Provider_Install_macOS.sh" ;;
esac

if [ "$URNETWORK_TOOLS_MODE" = "1" ]; then
    me="urnet-tools"
fi

provider_label="com.urnetwork.provider"
update_label="com.urnetwork.provider.update"
api_base="${URNETWORK_API_BASE:-https://api.github.com/repos/urnetwork/build}"
default_tools_url="https://raw.githubusercontent.com/urnetwork/connect/main/scripts/Provider_Install_macOS.sh"
tools_url="${URNETWORK_TOOLS_URL:-$default_tools_url}"
state_dir="$HOME/.urnetwork"
agents_dir="$HOME/Library/LaunchAgents"
log_dir="$HOME/Library/Logs/urnetwork"
log_max_bytes=10485760

if [ -n "$URNETWORK_PROVIDER_INSTALL" ]; then
    install_path="$URNETWORK_PROVIDER_INSTALL"
elif [ "$URNETWORK_TOOLS_MODE" = "1" ] && [ -f "$0" ]; then
    # urnet-tools lives in <install>/bin
    install_path="$(cd "$(dirname "$0")/.." && pwd)"
else
    install_path="$HOME/.local/share/urnetwork-provider"
fi

operation=""
from_installer=1
[ "$URNETWORK_TOOLS_MODE" = "1" ] && from_installer=0
dry_run=0
assume_yes=0

bold=""
reset=""
if [ -t 1 ]; then
    bold="$(printf '\033[1m')"
    reset="$(printf '\033[0m')"
fi

show_help ()
{
    if [ "$URNETWORK_TOOLS_MODE" = "1" ]; then
        cat <<EOF
Usage:
  $me start                 Start the provider in the background
  $me stop                  Stop the provider (it starts again at next login if auto-start is on)
  $me restart               Stop, then start the provider
  $me status                Show what is installed and whether the provider is running
  $me logs [-n LINES] [--no-follow]
                            Show the provider log (follows it by default)
  $me update                Install the latest release if it is newer
  $me reinstall [-t TAG]    Install the current (or the given) release again
  $me auto-update [on | off] [--interval INTERVAL]
                            Show or change automatic updates. INTERVAL is
                            6h, daily, weekly or monthly (default: 6h).
                            --interval alone keeps updates on or off as they are
  $me auto-start [on | off] Show or change starting the provider when you log in
  $me uninstall [--remove-identity] [-B] [-y]
                            Remove URnetwork. Your identity in ~/.urnetwork is kept
                            unless you pass --remove-identity.
  $me version               Show the installed version and the latest release

Options:
  -t, --tag TAG             Release tag to install, such as v2026.10.20 (default: latest)
  -B, --no-modify-path      Do not add or remove the PATH lines in your shell startup files
      --remove-identity     uninstall: also delete ~/.urnetwork (your provider identity);
                            asks you to type "delete" unless -y is given
  -y, --yes                 Do not ask questions (answers yes)
      --dry-run             Print what would change without changing anything
  -i, --install PATH        Installation path (default: ~/.local/share/urnetwork-provider)
  -h, --help                Show this help
  -v, --version             Same as \`version'

Both "--tag TAG" and "--tag=TAG" work, for every option that takes a value.
Logs: $log_dir
Docs: <https://ur.xyz/docs/miner>
EOF
    else
        cat <<EOF
Usage:
  $me [-t TAG] [--auth-code CODE] [-B] [-y] [--no-auto-update] [--no-start] [--dry-run] [-i PATH]

Installs the URnetwork provider for the current macOS user, runs it in the
background with launchd, and checks for updates every 6 hours.

Options:
  -t, --tag TAG             Release tag to install (default: latest)
  -B, --no-modify-path      Do not add the install folder to PATH in ~/.zshrc or ~/.bash_profile
      --auth-code CODE      Sign in with this auth code from https://ur.io
                            (or set URNETWORK_AUTH_CODE)
  -y, --yes                 Do not ask questions (no sign-in prompt)
      --no-auto-update      Install without automatic updates (turn on later with
                            \`urnet-tools auto-update on')
      --no-start            Do not sign in or start the provider after installing
      --dry-run             Print what would change without changing anything
  -i, --install PATH        Installation path (default: ~/.local/share/urnetwork-provider)
  -h, --help                Show this help

Both "--tag TAG" and "--tag=TAG" work.
After installing, manage everything with \`urnet-tools' (try \`urnet-tools --help').
Docs: <https://ur.xyz/docs/miner>
EOF
    fi
}

say ()
{
    printf '%s%s:%s %s\n' "$bold" "$me" "$reset" "$*"
}

warn ()
{
    printf '%s: warning: %s\n' "$me" "$*" >&2
}

die ()
{
    printf '%s: error: %s\n' "$me" "$*" >&2
    exit 1
}

# run prints a command instead of running it with --dry-run
run ()
{
    if [ "$dry_run" -eq 1 ]; then
        printf '[dry-run] %s\n' "$*"
        return 0
    fi
    "$@"
}

# run_quiet is run without the command's own output
run_quiet ()
{
    if [ "$dry_run" -eq 1 ]; then
        printf '[dry-run] %s\n' "$*"
        return 0
    fi
    "$@" > /dev/null 2>&1
}

# write_file PATH: stdin becomes PATH, atomically (a running urnet-tools keeps
# reading its old copy because the new one is a new file)
write_file ()
{
    wf_path="$1"
    if [ "$dry_run" -eq 1 ]; then
        printf '[dry-run] write %s\n' "$wf_path"
        cat > /dev/null
        return 0
    fi
    wf_tmp="$wf_path.tmp.$$"
    cat > "$wf_tmp" || { rm -f "$wf_tmp"; return 1; }
    mv -f "$wf_tmp" "$wf_path"
}

xml_escape ()
{
    printf '%s' "$1" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g' -e 's/"/\&quot;/g'
}

now_epoch ()
{
    date +%s
}

# 0..$1-1
random_below ()
{
    rb_n="$(od -An -N2 -tu2 /dev/urandom 2>/dev/null | tr -d ' ')"
    [ -n "$rb_n" ] || rb_n="$(( $$ + $(now_epoch) ))"
    echo $(( rb_n % $1 ))
}

# a subshell, because a failed redirection on `:' ends a POSIX shell
tty_ok ()
{
    (exec < /dev/tty) 2>/dev/null
}

ask_yes_no ()
{
    # $1 question, $2 default (y or n). Reads the terminal, so it also works
    # when the script itself arrives on stdin (curl | sh).
    if [ "$assume_yes" -eq 1 ]; then
        [ "$2" = y ]
        return
    fi
    if ! tty_ok; then
        [ "$2" = y ]
        return
    fi
    if [ "$2" = y ]; then
        printf '%s [Y/n] ' "$1" > /dev/tty
    else
        printf '%s [y/N] ' "$1" > /dev/tty
    fi
    read -r ayn_answer < /dev/tty || ayn_answer=""
    case "$ayn_answer" in
        "") [ "$2" = y ] ;;
        y|Y|yes|YES|Yes) return 0 ;;
        *) return 1 ;;
    esac
}

has_tty ()
{
    [ "$assume_yes" -eq 0 ] && [ -t 1 ] && tty_ok
}

# ---------------------------------------------------------------- settings

settings_file ()
{
    echo "$install_path/settings"
}

# get_setting KEY DEFAULT
get_setting ()
{
    gs_value=""
    if [ -f "$(settings_file)" ]; then
        gs_value="$(grep "^$1=" "$(settings_file)" 2>/dev/null | tail -n 1 | cut -d= -f2-)"
    fi
    if [ -z "$gs_value" ]; then
        echo "$2"
    else
        echo "$gs_value"
    fi
}

# set_setting KEY VALUE
set_setting ()
{
    if [ "$dry_run" -eq 1 ]; then
        printf '[dry-run] set %s=%s in %s\n' "$1" "$2" "$(settings_file)"
        return 0
    fi
    ss_file="$(settings_file)"
    {
        if [ -f "$ss_file" ]; then
            grep -v "^$1=" "$ss_file"
        fi
        echo "$1=$2"
    } > "$ss_file.tmp.$$" && mv -f "$ss_file.tmp.$$" "$ss_file"
}

# ---------------------------------------------------------------- platform

check_platform ()
{
    if [ "$(uname -s)" != "Darwin" ] && [ "$URNETWORK_ALLOW_NON_DARWIN" != "1" ]; then
        die "this installer is for macOS. On Linux use Provider_Install_Linux.sh, on Windows Provider_Install_Win32.ps1."
    fi

    if command -v sw_vers > /dev/null 2>&1; then
        cp_major="$(sw_vers -productVersion 2>/dev/null | cut -d. -f1)"
        case "$cp_major" in
            ''|*[!0-9]*) ;;
            *)
                if [ "$cp_major" -lt 12 ]; then
                    warn "macOS $(sw_vers -productVersion) is older than macOS 12 (Monterey); the provider may not run"
                fi
                ;;
        esac
    fi

    for cp_tool in curl tar shasum launchctl; do
        command -v "$cp_tool" > /dev/null 2>&1 || die "\`$cp_tool' is missing; it ships with macOS, so check your PATH"
    done
}

get_arch ()
{
    ga_machine="$(uname -m)"
    # A shell running under Rosetta reports x86_64 on Apple silicon; install
    # the native build anyway.
    if [ "$ga_machine" = "x86_64" ] && [ "$(sysctl -n sysctl.proc_translated 2>/dev/null)" = "1" ]; then
        ga_machine=arm64
    fi
    case "$ga_machine" in
        arm64|aarch64) echo arm64 ;;
        x86_64|amd64) echo amd64 ;;
        *) die "unsupported Mac architecture '$ga_machine' (supported: Apple silicon and Intel)" ;;
    esac
}

launchd_domain ()
{
    ld_uid="$(id -u)"
    # gui/<uid> exists while you are logged in at the screen. Over SSH without
    # a screen login, fall back to the background user domain.
    if launchctl print "gui/$ld_uid" > /dev/null 2>&1; then
        echo "gui/$ld_uid"
    else
        echo "user/$ld_uid"
    fi
}

# ---------------------------------------------------------------- network

fetch ()
{
    curl -fsSL --retry 3 --retry-delay 2 --connect-timeout 20 "$1"
}

# json_field KEY < JSON: a top-level string field
json_field ()
{
    jf_json="$(tr -d '\000-\037')"
    if command -v jq > /dev/null 2>&1; then
        printf '%s' "$jf_json" | jq -r --arg k "$1" '.[$k] // empty'
    elif command -v plutil > /dev/null 2>&1; then
        printf '%s' "$jf_json" | plutil -extract "$1" raw -o - - 2>/dev/null
    else
        printf '%s' "$jf_json" | sed -n "s/.*\"$1\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" | head -n 1
    fi
}

# release_asset ARCH < JSON: prints "URL DIGEST" of the provider tarball
# (DIGEST may be empty). A per-platform urnetwork-provider-<ver>-darwin-ARCH
# file wins over the all-platform urnetwork-provider-<ver> one.
release_asset ()
{
    ra_json="$(tr -d '\000-\037')"
    if command -v jq > /dev/null 2>&1; then
        printf '%s' "$ra_json" | jq -r --arg a "$1" '
            [.assets[]? | select(.name | startswith("urnetwork-provider-")) | select(.name | endswith(".tar.gz"))] as $all
            | ([$all[] | select(.name | endswith("-darwin-" + $a + ".tar.gz"))]
               + [$all[] | select(.name | test("-(linux|darwin|windows)-[a-z0-9]+[.]tar[.]gz$") | not)])[0] // empty
            | "\(.browser_download_url) \(.digest // "")"'
    elif command -v plutil > /dev/null 2>&1; then
        for ra_pass in platform all; do
            ra_i=0
            while ra_name="$(printf '%s' "$ra_json" | plutil -extract "assets.$ra_i.name" raw -o - - 2>/dev/null)"; do
                ra_i=$((ra_i + 1))
                case "$ra_pass:$ra_name" in
                    "platform:urnetwork-provider-"*"-darwin-$1.tar.gz") ;;
                    all:urnetwork-provider-*-linux-*.tar.gz|all:urnetwork-provider-*-darwin-*.tar.gz|all:urnetwork-provider-*-windows-*.tar.gz) continue ;;
                    all:urnetwork-provider-*.tar.gz) ;;
                    *) continue ;;
                esac
                ra_url="$(printf '%s' "$ra_json" | plutil -extract "assets.$((ra_i - 1)).browser_download_url" raw -o - - 2>/dev/null)"
                ra_digest="$(printf '%s' "$ra_json" | plutil -extract "assets.$((ra_i - 1)).digest" raw -o - - 2>/dev/null)"
                echo "$ra_url $ra_digest"
                return 0
            done
        done
    else
        ra_urls="$(printf '%s' "$ra_json" | grep -o '"browser_download_url"[[:space:]]*:[[:space:]]*"[^"]*urnetwork-provider-[^"]*\.tar\.gz"' |
            sed 's/.*"\(http[^"]*\)"$/\1/')"
        ra_url="$(printf '%s\n' "$ra_urls" | grep -- "-darwin-$1\.tar\.gz\$" | head -n 1)"
        [ -n "$ra_url" ] || ra_url="$(printf '%s\n' "$ra_urls" | grep -v -E -- '-(linux|darwin|windows)-[a-z0-9]+\.tar\.gz$' | head -n 1)"
        [ -n "$ra_url" ] && echo "$ra_url "
    fi
}

# ---------------------------------------------------------------- locking

lock_dir ()
{
    echo "$install_path/.lock"
}

take_lock ()
{
    [ "$dry_run" -eq 1 ] && return 0
    [ -d "$install_path" ] || created_install_path=1
    mkdir -p "$install_path" || die "could not create $install_path"
    tl_dir="$(lock_dir)"
    if ! mkdir "$tl_dir" 2>/dev/null; then
        tl_pid="$(cat "$tl_dir/pid" 2>/dev/null)"
        if [ -n "$tl_pid" ] && kill -0 "$tl_pid" 2>/dev/null; then
            die "another urnet-tools (pid $tl_pid) is installing or updating; try again in a minute"
        fi
        rm -rf "$tl_dir"
        mkdir "$tl_dir" 2>/dev/null || die "could not take the lock $tl_dir"
    fi
    echo $$ > "$tl_dir/pid"
    lock_held=1
}

release_lock ()
{
    if [ "${lock_held:-0}" -eq 1 ]; then
        rm -rf "$(lock_dir)"
        lock_held=0
    fi
}

cleanup ()
{
    # an update that failed after stopping the provider starts it again
    if [ "${restart_on_exit:-0}" -eq 1 ]; then
        restart_on_exit=0
        load_job "$provider_label" "$(provider_plist)" > /dev/null 2>&1 &&
            warn "the update failed; the provider was started again on the version it had"
    fi
    release_lock
    # a first install that failed before installing anything leaves nothing
    if [ "${created_install_path:-0}" -eq 1 ] && [ ! -e "$install_path/bin/urnetwork" ]; then
        rm -rf "$install_path"
    fi
    if [ -n "${workdir:-}" ] && [ -d "$workdir" ]; then
        rm -rf "$workdir"
    fi
}

trap cleanup EXIT
trap 'exit 1' INT TERM HUP

# ---------------------------------------------------------------- launchd

provider_plist_in_agents ()
{
    echo "$agents_dir/$provider_label.plist"
}

provider_plist_parked ()
{
    echo "$install_path/launchd/$provider_label.plist"
}

# Auto-start on keeps the provider plist in ~/Library/LaunchAgents, so launchd
# starts it at login. Auto-start off parks it in the install folder, where
# launchd never looks, and `urnet-tools start' loads it from there.
provider_plist ()
{
    if [ "$(get_setting auto_start 1)" = "1" ]; then
        provider_plist_in_agents
    else
        provider_plist_parked
    fi
}

update_plist ()
{
    echo "$agents_dir/$update_label.plist"
}

launchd_path_env="/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin:/usr/local/bin"

write_provider_plist ()
{
    wpp_tools="$(xml_escape "$install_path/bin/urnet-tools")"
    wpp_log="$(xml_escape "$log_dir/provider.log")"
    wpp_target="$(provider_plist)"
    run mkdir -p "$(dirname "$wpp_target")" || return 1
    # only one copy may exist
    if [ "$wpp_target" = "$(provider_plist_in_agents)" ]; then
        run rm -f "$(provider_plist_parked)"
    else
        run rm -f "$(provider_plist_in_agents)"
    fi
    write_file "$wpp_target" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>$provider_label</string>
    <key>ProgramArguments</key>
    <array>
        <string>$wpp_tools</string>
        <string>service-run</string>
    </array>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>$launchd_path_env</string>
    </dict>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>ThrottleInterval</key>
    <integer>60</integer>
    <key>StandardOutPath</key>
    <string>$wpp_log</string>
    <key>StandardErrorPath</key>
    <string>$wpp_log</string>
</dict>
</plist>
EOF
}

write_update_plist ()
{
    wup_tools="$(xml_escape "$install_path/bin/urnet-tools")"
    wup_log="$(xml_escape "$log_dir/update.log")"
    wup_hour="$(get_setting update_hour 3)"
    wup_minute="$(get_setting update_minute 0)"
    wup_when=""
    case "$(get_setting update_interval 6h)" in
        weekly)
            wup_when="        <dict>
            <key>Weekday</key>
            <integer>$(get_setting update_weekday 0)</integer>
            <key>Hour</key>
            <integer>$wup_hour</integer>
            <key>Minute</key>
            <integer>$wup_minute</integer>
        </dict>"
            ;;
        monthly)
            wup_when="        <dict>
            <key>Day</key>
            <integer>$(get_setting update_day 1)</integer>
            <key>Hour</key>
            <integer>$wup_hour</integer>
            <key>Minute</key>
            <integer>$wup_minute</integer>
        </dict>"
            ;;
        daily)
            wup_when="        <dict>
            <key>Hour</key>
            <integer>$wup_hour</integer>
            <key>Minute</key>
            <integer>$wup_minute</integer>
        </dict>"
            ;;
        *)
            for wup_h in $(update_hours); do
                wup_when="$wup_when${wup_when:+
}        <dict>
            <key>Hour</key>
            <integer>$wup_h</integer>
            <key>Minute</key>
            <integer>$wup_minute</integer>
        </dict>"
            done
            ;;
    esac
    run mkdir -p "$agents_dir" || return 1
    write_file "$(update_plist)" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>$update_label</string>
    <key>ProgramArguments</key>
    <array>
        <string>$wup_tools</string>
        <string>update</string>
        <string>--scheduled</string>
    </array>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>$launchd_path_env</string>
    </dict>
    <key>RunAtLoad</key>
    <true/>
    <key>StartCalendarInterval</key>
    <array>
$wup_when
    </array>
    <key>StandardOutPath</key>
    <string>$wup_log</string>
    <key>StandardErrorPath</key>
    <string>$wup_log</string>
</dict>
</plist>
EOF
}

# job_domain LABEL: the domain the job is loaded in, if any. An install over
# SSH loads into user/<uid>, and a later screen login can add a gui/<uid>
# copy, so every check looks at both.
job_domain ()
{
    jd_uid="$(id -u)"
    for jd_domain in "gui/$jd_uid" "user/$jd_uid"; do
        if launchctl print "$jd_domain/$1" > /dev/null 2>&1; then
            echo "$jd_domain"
            return 0
        fi
    done
    return 1
}

job_loaded ()
{
    job_domain "$1" > /dev/null
}

job_print ()
{
    jp_domain="$(job_domain "$1")" || return 1
    launchctl print "$jp_domain/$1" 2>/dev/null
}

job_running ()
{
    job_print "$1" | grep -q '^[[:space:]]*state = running'
}

job_pid ()
{
    job_print "$1" | sed -n 's/^[[:space:]]*pid = \([0-9]*\).*/\1/p' | head -n 1
}

job_last_exit ()
{
    job_print "$1" | sed -n 's/^[[:space:]]*last exit code = \(.*\)$/\1/p' | head -n 1
}

# load_job LABEL PLIST
load_job ()
{
    unload_job "$1"
    lj_domain="$(launchd_domain)"
    # a job disabled earlier (launchctl disable, or switched off under
    # System Settings > General > Login Items) refuses to bootstrap
    run_quiet launchctl enable "$lj_domain/$1"
    # right after a bootout launchd sometimes still answers 5 (I/O error)
    if ! run launchctl bootstrap "$lj_domain" "$2" 2>/dev/null; then
        sleep 1
        # from SSH, a gui/<uid> domain can refuse a bootstrap (error 125);
        # the background domain still takes it
        if [ "$lj_domain" != "user/$(id -u)" ] && run launchctl bootstrap "user/$(id -u)" "$2" 2>/dev/null; then
            return 0
        fi
        if ! run launchctl bootstrap "$lj_domain" "$2"; then
            warn "launchctl could not load $1 from $2"
            warn "if URnetwork (urnet-tools) is switched off under System Settings > General > Login Items, switch it on and run \`urnet-tools start'"
            return 1
        fi
    fi
}

unload_job ()
{
    while ul_domain="$(job_domain "$1")"; do
        run_quiet launchctl bootout "$ul_domain/$1"
        [ "$dry_run" -eq 1 ] && return 0
        ul_n=0
        while launchctl print "$ul_domain/$1" > /dev/null 2>&1 && [ "$ul_n" -lt 20 ]; do
            sleep 0.5 2>/dev/null || sleep 1
            ul_n=$((ul_n + 1))
        done
        if launchctl print "$ul_domain/$1" > /dev/null 2>&1; then
            warn "launchctl could not stop $1"
            return 1
        fi
    done
    return 0
}

# ---------------------------------------------------------------- PATH

path_block_start="# == urnetwork-provider start"
path_block_end="# == urnetwork-provider end"

# rc files that get the PATH lines: zsh is the macOS default shell, and
# Terminal starts bash as a login shell, which reads only the first of
# ~/.bash_profile, ~/.bash_login and ~/.profile
path_rc_files ()
{
    echo "$HOME/.zshrc"
    for prf_rc in "$HOME/.bash_profile" "$HOME/.bash_login" "$HOME/.profile"; do
        if [ -f "$prf_rc" ]; then
            case "${SHELL:-}" in
                */bash) echo "$prf_rc" ;;
                *) [ "$prf_rc" = "$HOME/.bash_profile" ] && echo "$prf_rc" ;;
            esac
            return 0
        fi
    done
    case "${SHELL:-}" in
        */bash) echo "$HOME/.bash_profile" ;;
    esac
    return 0
}

add_to_path ()
{
    path_rc_files | while IFS= read -r apt_rc; do
        if [ -f "$apt_rc" ] && grep -q "^$path_block_start\$" "$apt_rc"; then
            if grep -qF "export URNETWORK_PROVIDER_INSTALL=\"$install_path\"" "$apt_rc"; then
                say "Your $(basename "$apt_rc") already adds URnetwork to PATH"
                continue
            fi
            # an earlier install elsewhere: replace its lines
            remove_path_block "$apt_rc"
        fi
        say "Adding $install_path/bin to PATH in ~/$(basename "$apt_rc")"
        if [ "$dry_run" -eq 1 ]; then
            printf '[dry-run] append PATH block to %s\n' "$apt_rc"
            continue
        fi
        cat >> "$apt_rc" <<EOF

$path_block_start
export URNETWORK_PROVIDER_INSTALL="$install_path"
export PATH="\$PATH:\$URNETWORK_PROVIDER_INSTALL/bin"
$path_block_end
EOF
    done
}

# remove_path_block RC: drop our lines, keeping a backup
remove_path_block ()
{
    rpb_rc="$1"
    say "Removing the URnetwork PATH lines from ~/$(basename "$rpb_rc") (backup: ~/$(basename "$rpb_rc").urnetwork-backup)"
    if [ "$dry_run" -eq 1 ]; then
        return 0
    fi
    cp "$rpb_rc" "$rpb_rc.urnetwork-backup"
    awk -v s="$path_block_start" -v e="$path_block_end" '$0 == s { skip=1 } skip == 0 { print } $0 == e { skip=0 }' \
        "$rpb_rc.urnetwork-backup" > "$rpb_rc.tmp.$$" && cat "$rpb_rc.tmp.$$" > "$rpb_rc"
    rm -f "$rpb_rc.tmp.$$"
}

remove_from_path ()
{
    for rfp_rc in "$HOME/.zshrc" "$HOME/.bash_profile" "$HOME/.bash_login" "$HOME/.bashrc" "$HOME/.profile"; do
        [ -f "$rfp_rc" ] || continue
        grep -q "^$path_block_start\$" "$rfp_rc" || continue
        remove_path_block "$rfp_rc"
    done
}

# ---------------------------------------------------------------- install

# the identity folder must be private, or registration refuses to run
ensure_state_dir ()
{
    if [ ! -d "$state_dir" ]; then
        run mkdir -p "$state_dir" && run chmod 700 "$state_dir"
        return
    fi
    if [ -n "$(find "$state_dir" -maxdepth 0 \( -perm -020 -o -perm -002 \) 2>/dev/null)" ]; then
        say "Making $state_dir private (it was writable by other users)"
        run chmod go-w "$state_dir"
    fi
}

has_identity ()
{
    [ -s "$state_dir/jwt" ]
}

# a signed-in device becomes a provider client the first time it provides,
# which leaves .provider.jwt in the state folder
is_registered ()
{
    for ir_jwt in "$state_dir/.provider.jwt" "$state_dir"/.provider-*.jwt; do
        [ -s "$ir_jwt" ] && return 0
    done
    return 1
}

# job_exited LABEL: loaded, not running, and launchd has an exit code for it
job_exited ()
{
    ! job_running "$1" && case "$(job_last_exit "$1")" in [0-9]*|-[0-9]*) true ;; *) false ;; esac
}

# heal_registration: an install from before first-start registration, or
# one whose registration failed, has a signed-in provider job that exited
# without registering; an update starts it again so it registers
heal_registration ()
{
    if [ "$dry_run" -eq 0 ] && has_identity && ! is_registered && job_loaded "$provider_label" && ! job_running "$provider_label"; then
        say "This Mac is signed in but was never registered; starting the provider to register it"
        unload_job "$provider_label"
        start_provider quiet || true
    fi
}

# start_failed: say why the provider stopped right after starting
start_failed ()
{
    warn "$1; the last lines of the log:"
    tail -n 15 "$log_dir/provider.log" >&2 2>/dev/null
    if tail -n 30 "$log_dir/provider.log" 2>/dev/null | grep -q startup_recovery_required; then
        warn "the miner refused this sign-in. Sign in again with a new code from https://ur.io (\`urnetwork auth -f'), then \`urnet-tools start'"
    else
        warn "see \`urnet-tools logs' for more"
    fi
}

# provide_args: what launchd runs. The released miner registers a new device
# only with --allow-client-registration; without it the first start prints
# startup_recovery_required and exits 0, so launchd never retries. Pass it
# only until the device is registered (it never replaces an identity), and
# only to a miner that knows the flag.
provide_args ()
{
    if ! is_registered && "$install_path/bin/urnetwork" --help 2>&1 | grep -q -- '--allow-client-registration'; then
        echo "provide --allow-client-registration"
    else
        echo "provide"
    fi
}

# get_tools_script: the urnet-tools source to install, newest first
get_tools_script ()
{
    gts_out="$1"
    # an update fetches the published installer, so installer fixes reach
    # existing installs; a broken or missing download keeps the current copy
    # an installer run from a file installs that file
    if [ "$from_installer" -eq 1 ] && [ -f "$0" ] && grep -q '^# urnet-tools for macOS' "$0" 2>/dev/null; then
        cat "$0" > "$gts_out"
        return 0
    fi
    if [ -z "$URNETWORK_TOOLS_URL" ]; then
        tools_url="$(get_setting tools_url "$default_tools_url")"
    fi
    if [ "$URNETWORK_TOOLS_NO_SELF_UPDATE" != "1" ]; then
        if fetch "$tools_url" > "$gts_out.new" 2>/dev/null &&
            head -n 1 "$gts_out.new" | grep -q '^#!/bin/sh' &&
            grep -q '^# urnet-tools for macOS' "$gts_out.new" &&
            sh -n "$gts_out.new" 2>/dev/null; then
            mv -f "$gts_out.new" "$gts_out"
            return 0
        fi
        rm -f "$gts_out.new"
    fi
    if [ -f "$0" ] && grep -q '^# urnet-tools for macOS' "$0" 2>/dev/null; then
        cat "$0" > "$gts_out"
        return 0
    fi
    return 1
}

# stage_tools: writes bin/urnet-tools.new, ready to rename into place
stage_tools ()
{
    st_src="$workdir/urnet-tools.src"
    {
        head -n 1 "$st_src"
        echo "URNETWORK_TOOLS_MODE=1"
        tail -n +2 "$st_src" | grep -v '^URNETWORK_TOOLS_MODE=1$'
    } > "$install_path/bin/urnet-tools.new" &&
        chmod 755 "$install_path/bin/urnet-tools.new"
}

verify_sha256 ()
{
    vs_file="$1"
    vs_digest="$2"
    case "$vs_digest" in
        sha256:*) vs_want="${vs_digest#sha256:}" ;;
        *)
            # nobody is watching a scheduled update, so it never installs
            # a download it cannot check
            if [ "$scheduled" -eq 1 ]; then
                die "the release lists no checksum; skipping this automatic update (run \`urnet-tools update' to install it anyway)"
            fi
            say "The release lists no checksum for this download; skipping the checksum check"
            return 0
            ;;
    esac
    vs_have="$(shasum -a 256 "$vs_file" | cut -d' ' -f1)"
    if [ "$vs_have" != "$vs_want" ]; then
        die "the download is damaged: sha256 is $vs_have, the release says $vs_want"
    fi
    say "Checksum OK"
}

# prepare_binary FILE: make a downloaded binary runnable and prove it runs
prepare_binary ()
{
    pb_bin="$1"
    chmod 755 "$pb_bin"
    xattr -d com.apple.quarantine "$pb_bin" > /dev/null 2>&1
    if command -v codesign > /dev/null 2>&1 && ! codesign --verify "$pb_bin" > /dev/null 2>&1; then
        # Apple silicon kills unsigned binaries; an ad-hoc signature is enough
        codesign --force --sign - "$pb_bin" > /dev/null 2>&1 || warn "could not ad-hoc sign the provider binary"
    fi
    "$pb_bin" --help < /dev/null > /dev/null 2>&1
    pb_rc=$?
    if [ "$pb_rc" -ge 126 ]; then
        die "the downloaded provider does not run on this Mac (exit $pb_rc); nothing was changed"
    fi
}

do_install ()
{
    tag="latest"
    modify_path=1
    auto_update_on_install=1
    start_after=1
    auth_code="${URNETWORK_AUTH_CODE:-}"
    scheduled=0
    force=0

    while [ $# -gt 0 ]; do
        case "$1" in
            -t|--tag)
                [ -n "$2" ] || die "option '$1' needs a value, such as $1 v2026.10.20"
                tag="$2"
                shift 2
                ;;
            --tag=*)
                tag="${1#--tag=}"
                shift
                ;;
            -B|--no-modify-path|--no-modify-bashrc|--no_modify_bashrc)
                modify_path=0
                shift
                ;;
            --no-auto-update)
                [ "$operation" = "install" ] || die "option '$1' is only for the installer; use \`urnet-tools auto-update off'"
                auto_update_on_install=0
                shift
                ;;
            --auth-code)
                [ -n "$2" ] || die "option '$1' needs an auth code from https://ur.io"
                auth_code="$2"
                shift 2
                ;;
            --auth-code=*)
                auth_code="${1#--auth-code=}"
                shift
                ;;
            --no-start)
                [ "$operation" = "install" ] || die "option '$1' is only for the installer"
                start_after=0
                shift
                ;;
            --scheduled)
                [ "$operation" = "update" ] || die "option '$1' is only for update"
                scheduled=1
                shift
                ;;
            *)
                die "unknown option '$1' for $operation (try \`$me --help')"
                ;;
        esac
    done

    if [ -n "$auth_code" ] && [ "$operation" != "install" ]; then
        die "--auth-code is only for the installer; run \`urnetwork auth CODE'"
    fi
    if [ "$operation" = "update" ] && [ "$tag" != "latest" ]; then
        die "update always installs the latest release; use \`urnet-tools reinstall -t $tag'"
    fi
    if [ "$tag" != "latest" ]; then
        case "$tag" in
            v*) ;;
            *) tag="v$tag" ;;
        esac
        case "$tag" in
            */*|*..*) die "invalid tag '$tag'" ;;
        esac
    fi

    if [ "$operation" = "install" ] && [ -x "$install_path/bin/urnetwork" ]; then
        say "Found an existing installation in $install_path; updating it and keeping your settings"
        operation=reinstall
    fi
    if [ "$operation" = "reinstall" ]; then
        force=1
    fi

    if [ "$scheduled" -eq 1 ]; then
        if [ "$(get_setting auto_update 1)" != "1" ]; then
            say "Automatic updates are off; nothing to do"
            exit 0
        fi
        # the job also runs at every login so a Mac that was off at the
        # scheduled time catches up; skip if the last check is recent
        case "$(get_setting update_interval 6h)" in
            weekly) iu_gap=$((6 * 86400)) ;;
            monthly) iu_gap=$((27 * 86400)) ;;
            daily) iu_gap=$((20 * 3600)) ;;
            *) iu_gap=$((5 * 3600)) ;;
        esac
        iu_last="$(get_setting last_check 0)"
        case "$iu_last" in ''|*[!0-9]*) iu_last=0 ;; esac
        if [ $(( $(now_epoch) - iu_last )) -lt "$iu_gap" ]; then
            exit 0
        fi
        say "Scheduled update check at $(date)"
    fi

    check_platform
    arch="$(get_arch)" || exit 1

    if [ "$tag" = "latest" ]; then
        api_url="$api_base/releases/latest"
    else
        api_url="$api_base/releases/tags/$tag"
    fi

    say "Looking up release $tag"
    # a check at login can run before the network is up
    fr_n=0
    while ! release="$(fetch "$api_url")"; do
        fr_n=$((fr_n + 1))
        if [ "$scheduled" -eq 0 ] || [ "$fr_n" -ge 5 ]; then
            die "could not fetch release information from $api_url (check your internet connection)"
        fi
        sleep 60
    done
    version="$(printf '%s' "$release" | json_field tag_name)"
    [ -n "$version" ] || die "the release information has no tag_name"
    published="$(printf '%s' "$release" | json_field published_at)"

    installed_version="$(cat "$install_path/.version" 2>/dev/null)"
    if [ "$operation" = "update" ] && [ "$force" -eq 0 ] && [ "$version" = "$installed_version" ]; then
        say "Already up to date ($version)"
        [ "$dry_run" -eq 1 ] || set_setting last_check "$(now_epoch)"
        heal_registration
        exit 0
    fi

    asset="$(printf '%s' "$release" | release_asset "$arch")"
    dl_url="${asset%% *}"
    dl_digest="${asset#* }"
    [ "$dl_digest" = "$asset" ] && dl_digest=""
    [ -n "$dl_url" ] || die "release $version has no urnetwork-provider tarball"

    take_lock

    if [ "$dry_run" -eq 1 ]; then
        say "Would download $dl_url and install darwin/$arch/provider ($version) to $install_path/bin/urnetwork"
        workdir=""
    else
        workdir="$(mktemp -d "${TMPDIR:-/tmp}/urnetwork-install.XXXXXX")" || die "could not create a temporary folder"
        say "Downloading $version for $arch Macs"
        if [ -t 2 ]; then
            curl -fL --retry 3 --retry-delay 2 --progress-bar -o "$workdir/provider.tar.gz" "$dl_url" || die "download failed: $dl_url"
        else
            curl -fsSL --retry 3 --retry-delay 2 -o "$workdir/provider.tar.gz" "$dl_url" || die "download failed: $dl_url"
        fi
        verify_sha256 "$workdir/provider.tar.gz" "$dl_digest"
        # extract only this Mac's binary, not all twelve platforms; a
        # per-platform tarball may hold just `provider'
        mkdir "$workdir/x" || die "could not create a temporary folder"
        (cd "$workdir/x" && tar -xzf ../provider.tar.gz "darwin/$arch/provider" 2>/dev/null) ||
            (cd "$workdir/x" && tar -xzf ../provider.tar.gz 2>/dev/null) ||
            die "could not unpack the download"
        new_bin="$(find "$workdir/x" -type f -name provider -path "*darwin/$arch/*" | head -n 1)"
        [ -n "$new_bin" ] || new_bin="$(find "$workdir/x" -type f -name provider | grep -v -E '/(linux|windows|darwin)/' | head -n 1)"
        [ -n "$new_bin" ] || die "darwin/$arch/provider is missing from the release tarball"
        prepare_binary "$new_bin"
        # before anything changes, so a failure leaves the install as it was
        get_tools_script "$workdir/urnet-tools.src" || die "could not get the urnet-tools script (from $0 or $tools_url)"
    fi

    first_install=0
    [ -f "$(settings_file)" ] || first_install=1

    run mkdir -p "$install_path/bin" "$install_path/launchd" "$log_dir" || die "could not create $install_path"

    # settings survive updates; new installs pick a random update time so a
    # fleet does not hit the release server at once
    if [ "$first_install" -eq 1 ]; then
        set_setting auto_start 1
        set_setting auto_update "$auto_update_on_install"
        set_setting update_interval 6h
        set_setting update_hour "$(random_below 24)"
        set_setting update_minute "$(random_below 60)"
        set_setting update_weekday "$(random_below 7)"
        set_setting update_day "$(( $(random_below 28) + 1 ))"
    elif [ "$auto_update_on_install" -eq 0 ]; then
        set_setting auto_update 0
    fi
    # urnet-tools update keeps using the URL it was installed with
    if [ -n "$URNETWORK_TOOLS_URL" ]; then
        set_setting tools_url "$URNETWORK_TOOLS_URL"
    fi

    # stage everything first; the provider is only stopped for the renames
    if [ "$dry_run" -eq 0 ]; then
        if ! { cp "$new_bin" "$install_path/bin/urnetwork.new" &&
            chmod 755 "$install_path/bin/urnetwork.new"; }; then
            rm -f "$install_path/bin/urnetwork.new"
            die "could not copy the provider binary to $install_path/bin"
        fi
        stage_tools || die "could not write urnet-tools to $install_path/bin"
    fi
    write_provider_plist || die "could not write the provider launch agent"
    write_update_plist || die "could not write the update launch agent"

    was_running=0
    was_loaded=0
    job_loaded "$provider_label" && was_loaded=1
    if job_running "$provider_label"; then
        was_running=1
        restart_on_exit=1
        say "Stopping the provider for the update; it starts again when the update is done"
    fi
    unload_job "$provider_label" || die "could not stop the provider; nothing was changed"

    run mv -f "$install_path/bin/urnetwork.new" "$install_path/bin/urnetwork" || die "could not install the provider binary"
    run mv -f "$install_path/bin/urnet-tools.new" "$install_path/bin/urnet-tools" || die "could not install urnet-tools"
    if [ "$dry_run" -eq 0 ]; then
        echo "$version" > "$install_path/.version"
        echo "$published" > "$install_path/.date"
    fi
    set_setting last_check "$(now_epoch)"

    if [ "$(get_setting auto_update 1)" = "1" ]; then
        # the scheduled update runs as this job; reloading it would stop the
        # update half way, so it picks up the new plist at the next login
        if [ "$scheduled" -eq 0 ] || ! job_loaded "$update_label"; then
            load_job "$update_label" "$(update_plist)" || warn "automatic updates could not be turned on; try \`urnet-tools auto-update on'"
        fi
    else
        unload_job "$update_label"
        run rm -f "$(update_plist)"
    fi

    ensure_state_dir

    if [ "$modify_path" -eq 1 ] && [ "$from_installer" -eq 1 ]; then
        add_to_path
    fi

    release_lock

    if [ "$was_running" -eq 1 ]; then
        restart_on_exit=0
        start_provider quiet || warn "the provider did not start again; run \`urnet-tools start'"
    elif [ "$was_loaded" -eq 1 ] && [ "$from_installer" -eq 0 ]; then
        # it was loaded but had exited: retry, which also registers a
        # device that never registered
        heal_was_loaded=1
    fi
    if [ "${heal_was_loaded:-0}" -eq 1 ] && has_identity && ! is_registered; then
        say "This Mac is signed in but was never registered; starting the provider to register it"
        start_provider quiet || true
    fi
    say "Installed $version to $install_path"
    if [ "$tag" != "latest" ] && [ "$(get_setting auto_update 1)" = "1" ]; then
        say "Automatic updates will move you to the latest release; run \`urnet-tools auto-update off' to stay on $version"
    fi

    if [ "$from_installer" -eq 1 ]; then
        if ! has_identity && [ "$start_after" -eq 1 ] && [ "$dry_run" -eq 0 ]; then
            if [ -z "$auth_code" ] && has_tty; then
                printf '\n'
                printf 'Sign in to start mining. Paste an auth code from https://ur.io\n(or press Enter to skip and sign in later): ' > /dev/tty
                read -r auth_code < /dev/tty || auth_code=""
            fi
            if [ -n "$auth_code" ]; then
                "$install_path/bin/urnetwork" auth "$auth_code" < /dev/null ||
                    warn "sign-in failed; get a new auth code at https://ur.io and run \`urnetwork auth CODE'"
            fi
        fi
            if [ "$start_after" -eq 1 ] && has_identity; then
                start_provider quiet || true
            fi
            print_next_steps
    fi
}

print_next_steps ()
{
    printf '\n'
    if job_running "$provider_label"; then
        printf '%sThe provider is running in the background.%s\n' "$bold" "$reset"
    fi
    printf 'Open a new terminal window first, so the commands below are found.\n\n'
    if ! has_identity; then
        printf '  Sign in:          %surnetwork auth%s        # auth code from https://ur.io\n' "$bold" "$reset"
        printf '  Start mining:     %surnet-tools start%s\n' "$bold" "$reset"
    fi
    printf '  Check it:         %surnet-tools status%s\n' "$bold" "$reset"
    printf '  Watch the log:    %surnet-tools logs%s\n' "$bold" "$reset"
    printf '  Stop:             %surnet-tools stop%s\n' "$bold" "$reset"
    printf '  All commands:     %surnet-tools --help%s\n' "$bold" "$reset"
    printf '\n'
    if [ "$(get_setting auto_start 1)" = "1" ]; then
        printf 'It starts by itself when you log in.\n'
    fi
    if [ "$(get_setting auto_update 1)" = "1" ]; then
        printf 'It checks for updates %s, at %s.\n' "$(interval_text)" "$(update_time_text)"
    else
        printf 'Automatic updates are off; update with %surnet-tools update%s.\n' "$bold" "$reset"
    fi
    if [ "$(launchd_domain)" = "user/$(id -u)" ]; then
        printf '%sNo one is logged in at the screen, so after a restart the provider only starts once someone logs in.%s\n' "$bold" "$reset"
    fi
    printf 'macOS may show a "Background Items Added" notice for urnet-tools; that is expected.\n'
    printf '%sMore help: https://ur.xyz/docs/miner%s\n' "$bold" "$reset"
}

# ---------------------------------------------------------------- manage

require_install ()
{
    [ -x "$install_path/bin/urnetwork" ] || die "URnetwork is not installed in $install_path"
}

start_provider ()
{
    sp_quiet="$1"
    if job_running "$provider_label"; then
        [ "$sp_quiet" = quiet ] || say "The provider is already running (pid $(job_pid "$provider_label"))"
        return 0
    fi
    if ! has_identity; then
        say "Sign in first: run \`urnetwork auth' (get an auth code at https://ur.io), then \`urnet-tools start'"
        return 1
    fi
    sp_plist="$(provider_plist)"
    [ -f "$sp_plist" ] || write_provider_plist || return 1
    load_job "$provider_label" "$sp_plist" || return 1
    [ "$dry_run" -eq 1 ] && return 0
    sp_n=0
    while ! job_running "$provider_label" && ! job_exited "$provider_label" && [ "$sp_n" -lt 10 ]; do
        sleep 1
        sp_n=$((sp_n + 1))
    done
    # a first start registers the device; wait for that to finish, since a
    # refused registration exits 0 and launchd does not try again
    if ! is_registered; then
        say "Registering this Mac as a provider (first start)..."
        sp_n=0
        while job_running "$provider_label" && ! is_registered && [ "$sp_n" -lt 60 ]; do
            sleep 1
            sp_n=$((sp_n + 1))
        done
        if ! is_registered && ! job_running "$provider_label"; then
            start_failed "registration did not finish"
            return 1
        fi
        if ! is_registered; then
            say "Registration is still going; \`urnet-tools status' shows when it is done"
        fi
    else
        # a refused sign-in also exits 0 within seconds
        sp_n=0
        while job_running "$provider_label" && [ "$sp_n" -lt 5 ]; do
            sleep 1
            sp_n=$((sp_n + 1))
        done
    fi
    if job_running "$provider_label"; then
        say "The provider is running (pid $(job_pid "$provider_label")). Log: $log_dir/provider.log"
        return 0
    fi
    start_failed "the provider stopped right after starting"
    return 1
}

do_start ()
{
    [ $# -eq 0 ] || die "start takes no arguments"
    require_install
    start_provider
}

do_stop ()
{
    [ $# -eq 0 ] || die "stop takes no arguments"
    require_install
    if job_loaded "$provider_label"; then
        unload_job "$provider_label"
        if [ "$(get_setting auto_start 1)" = "1" ]; then
            say "Stopped. It starts again at your next login; \`urnet-tools auto-start off' prevents that"
        else
            say "Stopped"
        fi
    else
        say "The provider is not running"
    fi
}

do_restart ()
{
    [ $# -eq 0 ] || die "restart takes no arguments"
    require_install
    unload_job "$provider_label"
    start_provider
}

do_status ()
{
    [ $# -eq 0 ] || die "status takes no arguments"
    require_install
    ds_running=0
    printf 'Installed version:  %s\n' "$(cat "$install_path/.version" 2>/dev/null || echo unknown)"
    printf 'Install folder:     %s\n' "$install_path"
    if job_running "$provider_label"; then
        ds_running=1
        printf 'Provider:           %srunning%s (pid %s)\n' "$bold" "$reset" "$(job_pid "$provider_label")"
    elif job_loaded "$provider_label"; then
        if has_identity && ! is_registered; then
            printf 'Provider:           %snot running%s (device registration did not finish; see %surnet-tools logs%s, then %surnet-tools start%s)\n' "$bold" "$reset" "$bold" "$reset" "$bold" "$reset"
        elif has_identity && [ "$(job_last_exit "$provider_label")" = 0 ]; then
            printf 'Provider:           %sstopped%s (it exited; see %surnet-tools logs%s, then %surnet-tools start%s)\n' "$bold" "$reset" "$bold" "$reset" "$bold" "$reset"
        elif has_identity; then
            printf 'Provider:           %snot running%s (last exit code %s; launchd retries every minute, see %surnet-tools logs%s)\n' "$bold" "$reset" "$(job_last_exit "$provider_label")" "$bold" "$reset"
        else
            printf 'Provider:           %snot running%s (not signed in)\n' "$bold" "$reset"
        fi
    else
        printf 'Provider:           %sstopped%s (start it with %surnet-tools start%s)\n' "$bold" "$reset" "$bold" "$reset"
    fi
    if has_identity; then
        printf 'Signed in:          yes (%s)\n' "$state_dir/jwt"
        if is_registered; then
            printf 'Registered device:  yes\n'
        else
            printf 'Registered device:  not yet (the first start registers it)\n'
        fi
    else
        printf 'Signed in:          no (run %surnetwork auth%s)\n' "$bold" "$reset"
    fi
    if [ "$(get_setting auto_start 1)" = "1" ]; then
        printf 'Start at login:     on\n'
    else
        printf 'Start at login:     off\n'
    fi
    if [ "$(get_setting auto_update 1)" = "1" ]; then
        if job_loaded "$update_label"; then
            printf 'Automatic updates:  on, %s at %s (local time)\n' "$(interval_text)" "$(update_time_text)"
        else
            printf 'Automatic updates:  on, but not loaded (run %surnet-tools auto-update on%s)\n' "$bold" "$reset"
        fi
    else
        printf 'Automatic updates:  off\n'
    fi
    ds_last="$(get_setting last_check 0)"
    if [ "$ds_last" != "0" ]; then
        printf 'Last update check:  %s\n' "$(date -r "$ds_last" 2>/dev/null || date -d "@$ds_last" 2>/dev/null || echo "$ds_last")"
    fi
    printf 'Logs:               %s\n' "$log_dir"
    [ "$ds_running" -eq 1 ] || exit 3
}

interval_text ()
{
    case "$(get_setting update_interval 6h)" in
        6h) echo "every 6 hours" ;;
        *) get_setting update_interval 6h ;;
    esac
}

# the four hours of the 6h schedule, from the per-install random hour
update_hours ()
{
    uh_h="$(get_setting update_hour 3)"
    for uh_k in 0 1 2 3; do
        echo $(( (uh_h + uh_k * 6) % 24 ))
    done | sort -n | tr '\n' ' '
}

update_time_text ()
{
    utt="$(printf '%02d:%02d' "$(get_setting update_hour 3)" "$(get_setting update_minute 0)")"
    case "$(get_setting update_interval 6h)" in
        6h)
            utt=""
            for utt_h in $(update_hours); do
                utt="$utt${utt:+, }$(printf '%02d:%02d' "$utt_h" "$(get_setting update_minute 0)")"
            done
            ;;
        weekly)
            case "$(get_setting update_weekday 0)" in
                0|7) utt="Sundays $utt" ;; 1) utt="Mondays $utt" ;; 2) utt="Tuesdays $utt" ;;
                3) utt="Wednesdays $utt" ;; 4) utt="Thursdays $utt" ;; 5) utt="Fridays $utt" ;; 6) utt="Saturdays $utt" ;;
            esac
            ;;
        monthly) utt="day $(get_setting update_day 1) $utt" ;;
    esac
    echo "$utt"
}

do_logs ()
{
    dl_lines=50
    dl_follow=1
    while [ $# -gt 0 ]; do
        case "$1" in
            -n|--lines)
                [ -n "$2" ] || die "option '$1' needs a number"
                dl_lines="$2"
                shift 2
                ;;
            --lines=*) dl_lines="${1#--lines=}"; shift ;;
            --no-follow) dl_follow=0; shift ;;
            *) die "unknown option '$1' for logs" ;;
        esac
    done
    case "$dl_lines" in ''|*[!0-9]*) die "invalid number of lines '$dl_lines'" ;; esac
    dl_file="$log_dir/provider.log"
    [ -f "$dl_file" ] || die "no log yet at $dl_file (has the provider been started?)"
    if [ "$dl_follow" -eq 1 ] && [ -t 1 ]; then
        exec tail -n "$dl_lines" -f "$dl_file"
    fi
    tail -n "$dl_lines" "$dl_file"
}

do_version ()
{
    require_install
    dv_installed="$(cat "$install_path/.version" 2>/dev/null)"
    echo "Installed version: ${dv_installed:-unknown}"
    if dv_release="$(fetch "$api_base/releases/latest" 2>/dev/null)"; then
        dv_latest="$(printf '%s' "$dv_release" | json_field tag_name)"
        if [ -n "$dv_latest" ] && [ "$dv_latest" != "$dv_installed" ]; then
            echo "Latest release:    $dv_latest (run \`urnet-tools update')"
        elif [ -n "$dv_latest" ]; then
            echo "Latest release:    $dv_latest (up to date)"
        fi
    else
        warn "could not check the latest release"
    fi
}

do_auto_update ()
{
    dau_mode=""
    dau_interval=""
    while [ $# -gt 0 ]; do
        case "$1" in
            --interval)
                [ -n "$2" ] || die "option '$1' needs a value: 6h, daily, weekly or monthly"
                dau_interval="$2"
                shift 2
                ;;
            --interval=*) dau_interval="${1#--interval=}"; shift ;;
            on|off)
                [ -z "$dau_mode" ] || die "unexpected argument '$1'"
                dau_mode="$1"
                shift
                ;;
            *) die "unknown argument '$1' for auto-update (use on, off, --interval)" ;;
        esac
    done
    case "$dau_interval" in
        ""|6h|daily|weekly|monthly) ;;
        *) die "invalid interval '$dau_interval': use 6h, daily, weekly or monthly" ;;
    esac
    require_install

    if [ -z "$dau_mode" ] && [ -z "$dau_interval" ]; then
        if [ "$(get_setting auto_update 1)" = "1" ]; then
            say "Automatic updates are on: $(interval_text) at $(update_time_text)"
        else
            say "Automatic updates are off"
        fi
        return 0
    fi
    # --interval alone changes the interval and keeps updates on or off
    if [ -z "$dau_mode" ]; then
        set_setting update_interval "$dau_interval"
        if [ "$(get_setting auto_update 1)" != "1" ]; then
            say "Saved: $(interval_text). Automatic updates stay off; \`urnet-tools auto-update on' turns them on"
            return 0
        fi
        dau_mode=on
    fi

    case "$dau_mode" in
        on)
            [ -n "$dau_interval" ] && set_setting update_interval "$dau_interval"
            set_setting auto_update 1
            write_update_plist || die "could not write $(update_plist)"
            load_job "$update_label" "$(update_plist)" || die "could not turn on automatic updates"
            say "Automatic updates are on: $(interval_text) at $(update_time_text)"
            ;;
        off)
            set_setting auto_update 0
            unload_job "$update_label"
            run rm -f "$(update_plist)"
            say "Automatic updates are off. Update by hand with \`urnet-tools update'"
            ;;
    esac
}

do_auto_start ()
{
    [ $# -le 1 ] || die "auto-start takes one argument: on or off"
    require_install
    case "${1:-}" in
        "")
            if [ "$(get_setting auto_start 1)" = "1" ]; then
                say "The provider starts when you log in"
            else
                say "The provider does not start when you log in"
            fi
            ;;
        on|off)
            if [ "$1" = on ]; then set_setting auto_start 1; else set_setting auto_start 0; fi
            # moving the plist does not stop or start the running provider
            write_provider_plist || die "could not write the provider launch agent"
            if [ "$1" = on ]; then
                say "The provider will start when you log in"
            else
                say "The provider will not start when you log in (\`urnet-tools start' still works)"
            fi
            ;;
        *) die "invalid value '$1': use on or off" ;;
    esac
}

do_uninstall ()
{
    du_identity=0
    du_path=1
    while [ $# -gt 0 ]; do
        case "$1" in
            --remove-identity) du_identity=1; shift ;;
            -B|--no-modify-path|--no-modify-bashrc|--no_modify_bashrc) du_path=0; shift ;;
            -y|--yes) assume_yes=1; shift ;;
            *) die "unknown option '$1' for uninstall" ;;
        esac
    done
    [ -f "$install_path/bin/urnetwork" ] || [ -f "$install_path/settings" ] ||
        die "nothing to uninstall: no URnetwork install in $install_path"

    if [ "$du_identity" -eq 1 ] && [ -d "$state_dir" ]; then
        if [ "$assume_yes" -eq 0 ]; then
            if ! tty_ok; then
                die "--remove-identity deletes $state_dir for good; add -y to confirm without a terminal"
            fi
            printf 'This deletes %s, your provider identity, for good.\nType delete to confirm: ' "$state_dir" > /dev/tty
            read -r du_answer < /dev/tty || du_answer=""
            [ "$du_answer" = delete ] || die "kept everything; nothing was removed"
        fi
    fi

    take_lock
    say "Stopping the provider and automatic updates"
    unload_job "$provider_label"
    unload_job "$update_label"
    run rm -f "$(provider_plist_in_agents)" "$(update_plist)"
    release_lock
    say "Removing $install_path"
    # only what the installer put there, never the folder as a whole: the
    # path may be one the user chose with -i
    run rm -f "$install_path/bin/urnetwork" "$install_path/bin/urnet-tools" \
        "$install_path/bin/urnetwork.new" "$install_path/bin/urnet-tools.new" \
        "$install_path/settings" "$install_path/.version" "$install_path/.date" \
        "$(provider_plist_parked)"
    run rm -rf "$install_path/.lock"
    run rmdir "$install_path/launchd" "$install_path/bin" "$install_path" 2>/dev/null
    [ -d "$install_path" ] && [ "$dry_run" -eq 0 ] && say "Left $install_path in place because it has other files in it"
    run rm -f "$log_dir/provider.log" "$log_dir/provider.log.1" "$log_dir/update.log" "$log_dir/update.log.1"
    run rmdir "$log_dir" 2>/dev/null
    if [ "$du_path" -eq 1 ]; then
        remove_from_path
    fi
    if [ "$du_identity" -eq 1 ]; then
        say "Removing your identity in $state_dir"
        run rm -rf "$state_dir"
    elif [ -d "$state_dir" ]; then
        say "Kept your identity in $state_dir; reinstalling picks it up again"
        say "(to delete it too: rm -rf ~/.urnetwork)"
    fi
    say "Uninstalled"
}

# launchd runs this; it exits 0 without an identity so launchd does not keep
# restarting a provider that cannot sign in
do_service_run ()
{
    # launchd opened the logs before this runs, so copy and truncate them in
    # place rather than renaming them
    for dsr_log in "$log_dir/provider.log" "$log_dir/update.log"; do
        if [ -f "$dsr_log" ] && [ "$(wc -c < "$dsr_log")" -gt "$log_max_bytes" ]; then
            cp -f "$dsr_log" "$dsr_log.1" && : > "$dsr_log"
        fi
    done
    if ! has_identity; then
        echo "$(date): not signed in; run \`urnetwork auth', then \`urnet-tools start'"
        exit 0
    fi
    dsr_args="$(provide_args)"
    if ! is_registered; then
        echo "$(date): first start on this device: registering it as a provider ($dsr_args)"
    fi
    # shellcheck disable=SC2086
    exec "$install_path/bin/urnetwork" $dsr_args
}

# ---------------------------------------------------------------- main

# refuse paths that would put the install on top of the user's own files, or
# that cannot be quoted safely in plists and shell startup files
check_install_path ()
{
    case "$install_path" in
        ""|/|"$HOME"|/usr|/usr/local|/opt|/opt/homebrew|/Applications|/Library|/System|"$HOME/Library"|"$HOME/.local"|"$HOME/.local/share"|"$state_dir")
            die "refusing to install into $install_path; pick a folder of its own, such as ~/.local/share/urnetwork-provider"
            ;;
    esac
    case "$install_path" in
        *'"'*|*'$'*|*'`'*|*\\*)
            die 'the install path must not contain quotes, dollar signs, backticks or backslashes'
            ;;
    esac
    if [ "$(printf '%s' "$install_path" | wc -l | tr -d ' ')" != "0" ]; then
        die "the install path must not contain a line break"
    fi
}

args_rest=""
add_arg ()
{
    # keep arguments intact through `set --'
    args_rest="$args_rest '$(printf '%s' "$1" | sed "s/'/'\\\\''/g")'"
}

# everything runs from here, on the last line, so a download cut off half
# way runs nothing
main ()
{
    while [ $# -gt 0 ]; do
        case "$1" in
            -h|--help)
                show_help
                exit 0
                ;;
            -v|--version)
                [ -z "$operation" ] && operation=version
                shift
                ;;
            -t|--tag|-n|--lines|--interval)
                # options with a value, so the value is not taken for a command
                add_arg "$1"
                if [ $# -gt 1 ]; then add_arg "$2"; shift; fi
                shift
                ;;
            -i|--install)
                [ -n "$2" ] || die "option '$1' needs a path"
                install_path="$2"
                shift 2
                ;;
            --install=*)
                install_path="${1#--install=}"
                shift
                ;;
            --dry-run)
                dry_run=1
                shift
                ;;
            -y|--yes)
                assume_yes=1
                shift
                ;;
            --)
                shift
                while [ $# -gt 0 ]; do add_arg "$1"; shift; done
                ;;
            -*)
                add_arg "$1"
                shift
                ;;
            *)
                if [ -z "$operation" ] && [ "$URNETWORK_TOOLS_MODE" = "1" ]; then
                    operation="$1"
                else
                    add_arg "$1"
                fi
                shift
                ;;
        esac
    done
    eval "set -- $args_rest"

    if [ "$URNETWORK_TOOLS_MODE" != "1" ]; then
        if [ "$operation" = version ]; then
            die "this is the installer; once installed, \`urnet-tools version' shows the version"
        fi
        operation=install
    fi

    if [ -z "$operation" ]; then
        show_help >&2
        exit 1
    fi

    case "$install_path" in
        /*) ;;
        *) install_path="$script_rundir/$install_path" ;;
    esac
    install_path="${install_path%/}"
    check_install_path

    if [ "$(id -u)" -eq 0 ] && [ "$URNETWORK_ALLOW_ROOT" != "1" ]; then
        die "run this without sudo; it installs URnetwork for your own user"
    fi

    case "$operation" in
        install|update|reinstall) do_install "$@" ;;
        uninstall) do_uninstall "$@" ;;
        start) do_start "$@" ;;
        stop) do_stop "$@" ;;
        restart) do_restart "$@" ;;
        status) do_status "$@" ;;
        logs|log) do_logs "$@" ;;
        version) do_version ;;
        auto-update) do_auto_update "$@" ;;
        auto-start) do_auto_start "$@" ;;
        service-run) do_service_run ;;
        *) die "unknown command '$operation' (try \`$me --help')" ;;
    esac
}

main "$@"
