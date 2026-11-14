#!/bin/sh
# Runs Provider_Install_macOS.sh end to end against a fake macOS: uname,
# sw_vers, sysctl, launchctl, curl, xattr and codesign are stubs, the release
# is a local tarball with a fake provider, and HOME is a temporary folder.
# Usage: sh Provider_Install_macOS.test.sh [shell...]   (default: sh)
set -u

here="$(cd "$(dirname "$0")" && pwd)"
script="$here/Provider_Install_macOS.sh"
shells="${*:-sh}"
failures=0

fail ()
{
    echo "FAIL [$sh_name] $*" >&2
    failures=$((failures + 1))
}

setup ()
{
    run_dir="$(mktemp -d "${TMPDIR:-/tmp}/urnetwork-macos.test.XXXXXX")"
    # a space and an ampersand prove paths are quoted and escaped in plists
    export HOME="$run_dir/home dir & co"
    mkdir -p "$HOME" "$run_dir/bin" "$run_dir/launchd" "$run_dir/fixtures"
    : > "$run_dir/calls"
    export MOCK_DIR="$run_dir"
    export MOCK_ARCH=arm64
    export MOCK_TRANSLATED=0
    export MOCK_GUI=1
    export MOCK_UID=501
    export SHELL=/bin/zsh
    unset URNETWORK_PROVIDER_INSTALL URNETWORK_TOOLS_MODE
    export URNETWORK_API_BASE="https://api.test/repos/urnetwork/build"
    export URNETWORK_TOOLS_URL="https://raw.test/Provider_Install_macOS.sh"

    cat > "$run_dir/bin/uname" <<'EOF'
#!/bin/sh
case "$1" in -s) echo "${MOCK_UNAME:-Darwin}" ;; -m) echo "$MOCK_ARCH" ;; *) echo Darwin ;; esac
EOF
    cat > "$run_dir/bin/id" <<'EOF'
#!/bin/sh
if [ "$1" = -u ] && [ -n "$MOCK_UID" ]; then echo "$MOCK_UID"; exit 0; fi
exec /usr/bin/id "$@"
EOF
    cat > "$run_dir/bin/sw_vers" <<'EOF'
#!/bin/sh
echo 15.1
EOF
    cat > "$run_dir/bin/sysctl" <<'EOF'
#!/bin/sh
echo "$MOCK_TRANSLATED"
EOF
    cat > "$run_dir/bin/xattr" <<'EOF'
#!/bin/sh
echo "xattr $*" >> "$MOCK_DIR/calls"
EOF
    cat > "$run_dir/bin/codesign" <<'EOF'
#!/bin/sh
echo "codesign $*" >> "$MOCK_DIR/calls"
case "$1" in --verify) exit 1 ;; esac
EOF
    # launchd: a job is a file in $MOCK_DIR/launchd; a bootstrapped job runs
    # (RunAtLoad), and bootstrap checks the plist is valid
    cat > "$run_dir/bin/launchctl" <<'EOF'
#!/bin/sh
echo "launchctl $*" >> "$MOCK_DIR/calls"
d="$MOCK_DIR/launchd"
case "$1" in
    print)
        case "$2" in
            gui/*/*|user/*/*)
                l="${2##*/}"
                [ -f "$d/$l" ] || exit 113
                [ "$(cat "$d/$l.domain")" = "${2%/*}" ] || exit 113
                if [ -f "$d/$l.exited" ]; then
                    printf '%s = {\n\tstate = not running\n\tlast exit code = %s\n}\n' "$l" "$(cat "$d/$l.exited" 2>/dev/null | grep . || echo 1)"
                else
                    printf '%s = {\n\tstate = running\n\tpid = 4242\n\tlast exit code = 0\n}\n' "$l"
                fi
                ;;
            gui/*) [ "$MOCK_GUI" = 1 ] || exit 113 ;;
            user/*) ;;
        esac
        ;;
    bootstrap)
        if command -v python3 > /dev/null; then
            python3 -I -c 'import plistlib,sys; p=plistlib.load(open(sys.argv[1],"rb")); open(sys.argv[2],"w").write(p["Label"]+"\n"+"\n".join(p["ProgramArguments"]))' "$3" "$d/.last" || exit 5
        else
            plutil -lint "$3" > /dev/null || exit 5
            sed -n 's:.*<string>\(.*\)</string>.*:\1:p' "$3" | sed -n '1,4p' | sed 's/&amp;/\&/g' > "$d/.last"
        fi
        l="$(head -n 1 "$d/.last")"
        [ -f "$d/$l.disabled" ] && exit 5
        [ -f "$d/$l" ] && exit 5
        mv "$d/.last" "$d/$l"
        echo "$2" > "$d/$l.domain"
        # the provider's first start registers the device, or fails to
        if [ "$l" = com.urnetwork.provider ] && [ -s "$HOME/.urnetwork/jwt" ]; then
            if [ "${MOCK_REG_FAIL:-0}" = 1 ] || [ "${MOCK_SIGNIN_REFUSED:-0}" = 1 ]; then
                echo 0 > "$d/$l.exited"
                echo "startup_recovery_required" >> "$HOME/Library/Logs/urnetwork/provider.log"
            elif [ ! -s "$HOME/.urnetwork/.provider.jwt" ]; then
                echo registered > "$HOME/.urnetwork/.provider.jwt"
            fi
        fi
        ;;
    bootout) l="${2##*/}"; [ -f "$d/$l" ] || exit 3; [ "$(cat "$d/$l.domain")" = "${2%/*}" ] || exit 3; rm -f "$d/$l" "$d/$l.exited" ;;
    enable) rm -f "$d/${2##*/}.disabled" ;;
    disable) touch "$d/${2##*/}.disabled" ;;
esac
EOF
    # curl: releases and downloads come from $MOCK_DIR/fixtures
    cat > "$run_dir/bin/curl" <<'EOF'
#!/bin/sh
out=""
url=""
while [ $# -gt 0 ]; do
    case "$1" in
        -o) out="$2"; shift 2 ;;
        --retry|--retry-delay|--connect-timeout) shift 2 ;;
        -*) shift ;;
        *) url="$1"; shift ;;
    esac
done
echo "curl $url" >> "$MOCK_DIR/calls"
f="$MOCK_DIR/fixtures"
case "$url" in
    */releases/latest) src="$f/latest.json" ;;
    */releases/tags/*) src="$f/tag-${url##*/}.json" ;;
    https://dl.test/*) src="$f/${url##*/}" ;;
    https://raw.test/*) src="$f/tools.sh" ;;
    *) exit 6 ;;
esac
[ -f "$src" ] || exit 22
if [ -n "$out" ]; then cp "$src" "$out"; else cat "$src"; fi
EOF
    chmod +x "$run_dir/bin/"*
    export PATH="$run_dir/bin:$PATH"
}

# make_release TAG [BROKEN]: a release JSON and tarball for TAG
make_release ()
{
    mr_tag="$1"
    mr_root="$run_dir/build-$mr_tag"
    for mr_arch in arm64 amd64; do
        mkdir -p "$mr_root/darwin/$mr_arch" "$mr_root/linux/amd64"
        cat > "$mr_root/darwin/$mr_arch/provider" <<EOF
#!/bin/sh
# fake provider $mr_tag $mr_arch
case "\$1" in
    --help) [ "${2:-}" = broken ] && exit 137; echo "provider provide [--allow-client-registration]"; exit 0 ;;
    auth) mkdir -p "\$HOME/.urnetwork"; echo "token \${2:-}" > "\$HOME/.urnetwork/jwt" ;;
    provide)
        if [ "\${2:-}" = --allow-client-registration ]; then
            echo registered > "\$HOME/.urnetwork/.provider.jwt"
            echo "registering"
        fi
        echo "providing $mr_tag $mr_arch \$*"
        ;;
esac
EOF
        chmod 755 "$mr_root/darwin/$mr_arch/provider"
    done
    echo linux > "$mr_root/linux/amd64/provider"
    tar -czf "$run_dir/fixtures/urnetwork-provider-$mr_tag.tar.gz" -C "$mr_root" darwin linux
    mr_sum="$(shasum -a 256 "$run_dir/fixtures/urnetwork-provider-$mr_tag.tar.gz" | cut -d' ' -f1)"
    [ "${3:-}" = badsum ] && mr_sum="0000000000000000000000000000000000000000000000000000000000000000"
    mr_digest=", \"digest\": \"sha256:$mr_sum\""
    [ "${3:-}" = nodigest ] && mr_digest=""
    cat > "$run_dir/fixtures/tag-$mr_tag.json" <<EOF
{"tag_name": "$mr_tag", "published_at": "2026-10-20T00:00:00Z",
 "assets": [{"name": "urnetwork-provider-$mr_tag.tar.gz.sig", "browser_download_url": "https://dl.test/nope"},
            {"name": "urnetwork-provider-$mr_tag.tar.gz", "browser_download_url": "https://dl.test/urnetwork-provider-$mr_tag.tar.gz"$mr_digest}]}
EOF
    cp "$run_dir/fixtures/tag-$mr_tag.json" "$run_dir/fixtures/latest.json"
}

install_dir () { echo "$HOME/.local/share/urnetwork-provider"; }
tools () { URNETWORK_PROVIDER_INSTALL="" $sh_cmd "$(install_dir)/bin/urnet-tools" "$@"; }
loaded () { [ -f "$run_dir/launchd/$1" ]; }
setting () { grep "^$1=" "$(install_dir)/settings" | cut -d= -f2-; }

run_tests ()
{
    setup
    make_release v1

    # --- fresh install without an identity
    $sh_cmd "$script" -y > "$run_dir/out" 2>&1 || fail "install failed: $(cat "$run_dir/out")"
    [ -x "$(install_dir)/bin/urnetwork" ] || fail "provider binary not installed"
    grep -q "fake provider v1 arm64" "$(install_dir)/bin/urnetwork" || fail "wrong binary installed"
    sed -n 2p "$(install_dir)/bin/urnet-tools" | grep -q '^URNETWORK_TOOLS_MODE=1$' || fail "urnet-tools mode line missing"
    [ "$(cat "$(install_dir)/.version")" = v1 ] || fail "version file"
    loaded com.urnetwork.provider.update || fail "update agent not loaded right after install (the Linux --now bug)"
    loaded com.urnetwork.provider && fail "provider started without an identity"
    [ -f "$HOME/Library/LaunchAgents/com.urnetwork.provider.plist" ] || fail "provider plist missing"
    grep -q "urnetwork-provider start" "$HOME/.zshrc" 2>/dev/null || fail "PATH not added to a new ~/.zshrc"
    [ -f "$HOME/.bash_profile" ] && fail "bash_profile created for a zsh user"
    [ -n "$(find "$HOME/.urnetwork" -maxdepth 0 -perm 700)" ] || fail "state dir not private"
    grep -q "codesign --force --sign -" "$run_dir/calls" || fail "unsigned binary was not ad-hoc signed"
    grep -q "xattr -d com.apple.quarantine" "$run_dir/calls" || fail "quarantine not cleared"
    grep -q "urnetwork auth" "$run_dir/out" || fail "next steps do not mention sign-in"
    [ -d "$HOME/linux" ] && fail "extracted other platforms"
    [ "$(setting update_interval)" = 6h ] || fail "default interval is not 6h"
    [ "$(grep -c "<key>Hour</key>" "$HOME/Library/LaunchAgents/com.urnetwork.provider.update.plist")" = 4 ] || fail "6h schedule does not have 4 times"
    hour="$(setting update_hour)"
    [ -n "$hour" ] || fail "no update hour"
    grep -q "<string>$HOME/.local/share/urnetwork-provider/bin/urnet-tools</string>" "$HOME/Library/LaunchAgents/com.urnetwork.provider.plist" &&
        fail "the & in HOME was not escaped in the plist"
    grep -q "home dir &amp; co" "$HOME/Library/LaunchAgents/com.urnetwork.provider.plist" || fail "plist path not escaped"
    sed -n 2,4p "$run_dir/launchd/com.urnetwork.provider.update" | tr '\n' ' ' | grep -q "^$(install_dir)/bin/urnet-tools update --scheduled" ||
        fail "update agent arguments: $(cat "$run_dir/launchd/com.urnetwork.provider.update")"

    # --- running the installer again keeps settings and PATH lines single
    $sh_cmd "$script" -y > "$run_dir/out" 2>&1 || fail "second install failed: $(cat "$run_dir/out")"
    [ "$(grep -c "urnetwork-provider start" "$HOME/.zshrc")" = 1 ] || fail "PATH lines duplicated"
    [ "$(setting update_hour)" = "$hour" ] || fail "update hour changed on reinstall"

    # --- start, status, stop
    tools start > "$run_dir/out" 2>&1 && fail "start without identity succeeded"
    grep -q "urnetwork auth" "$run_dir/out" || fail "start without identity gave no hint"
    "$(install_dir)/bin/urnetwork" auth
    MOCK_REG_FAIL=1 tools start > "$run_dir/out" 2>&1 && fail "start with a failed registration succeeded"
    grep -q "registration did not finish" "$run_dir/out" || fail "failed registration not reported: $(cat "$run_dir/out")"
    grep -q "startup_recovery_required" "$run_dir/out" || fail "failed registration did not show the log"
    tools status 2>&1 | grep -q "registration did not finish" || fail "status after a failed registration"
    tools stop > /dev/null 2>&1
    tools start > "$run_dir/out" 2>&1 || fail "start failed: $(cat "$run_dir/out")"
    [ -s "$HOME/.urnetwork/.provider.jwt" ] || fail "start did not register the device"
    # a registered device whose sign-in the miner refuses (e.g. another account)
    tools stop > /dev/null 2>&1
    MOCK_SIGNIN_REFUSED=1 tools start > "$run_dir/out" 2>&1 && fail "start with a refused sign-in succeeded"
    grep -q "urnetwork auth -f" "$run_dir/out" || fail "refused sign-in gave no hint: $(cat "$run_dir/out")"
    tools status 2>&1 | grep -q "it exited" || fail "status after an exit 0 promises a retry"
    tools stop > /dev/null 2>&1
    tools start > /dev/null 2>&1 || fail "start after a refused sign-in"
    loaded com.urnetwork.provider || fail "provider not loaded by start"
    tools start > "$run_dir/out" 2>&1 || fail "start when running must succeed"
    tools status > "$run_dir/out" 2>&1 || fail "status of a running provider exit $?"
    grep -q "running" "$run_dir/out" || fail "status does not say running"
    tools stop > /dev/null 2>&1 || fail "stop failed"
    loaded com.urnetwork.provider && fail "stop left the provider loaded"
    tools status > /dev/null 2>&1
    [ $? -eq 3 ] || fail "status of a stopped provider must exit 3"
    tools start > /dev/null 2>&1 || fail "second start failed"

    # --- update: same version is a no-op
    : > "$run_dir/calls"
    tools update > "$run_dir/out" 2>&1 || fail "update failed"
    grep -q "Already up to date" "$run_dir/out" || fail "no up-to-date message"
    grep -q "bootout" "$run_dir/calls" && fail "up-to-date update touched launchd"

    # --- settings survive an update; a running provider is restarted
    tools auto-update on --interval weekly > /dev/null 2>&1 || fail "auto-update weekly"
    grep -q "<key>Weekday</key>" "$HOME/Library/LaunchAgents/com.urnetwork.provider.update.plist" || fail "weekly schedule missing"
    tools auto-update > "$run_dir/out" 2>&1
    grep -q "weekly" "$run_dir/out" || fail "auto-update state does not show weekly"
    make_release v2
    printf '%s\n' "#!/bin/sh" "# urnet-tools for macOS -- self-update test" "echo selfupdated" > "$run_dir/fixtures/tools.sh"
    tools update > "$run_dir/out" 2>&1 || fail "update to v2 failed: $(cat "$run_dir/out")"
    grep -q "fake provider v2" "$(install_dir)/bin/urnetwork" || fail "binary not updated"
    [ "$(setting update_interval)" = weekly ] || fail "update reset the interval (Linux bug)"
    loaded com.urnetwork.provider || fail "provider not restarted after update"
    loaded com.urnetwork.provider.update || fail "update agent not loaded after update"
    grep -q "^echo selfupdated" "$(install_dir)/bin/urnet-tools" || fail "urnet-tools did not update itself"
    sed -n 2p "$(install_dir)/bin/urnet-tools" | grep -q '^URNETWORK_TOOLS_MODE=1$' || fail "self-updated tools lost mode line"
    # put the real tools back for the rest of the tests
    rm -f "$run_dir/fixtures/tools.sh"
    $sh_cmd "$script" -y > /dev/null 2>&1 || fail "reinstall from file failed"
    grep -q "^echo selfupdated" "$(install_dir)/bin/urnet-tools" && fail "installer run from a file did not install that file"

    # --- a broken tools download keeps the current copy
    printf '%s\n' "#!/bin/sh" "# urnet-tools for macOS" "if then fi (" > "$run_dir/fixtures/tools.sh"
    tools reinstall > "$run_dir/out" 2>&1 || fail "reinstall failed: $(cat "$run_dir/out")"
    grep -q "^provider_label=" "$(install_dir)/bin/urnet-tools" || fail "broken self-update replaced urnet-tools"
    rm -f "$run_dir/fixtures/tools.sh"

    # --- the scheduled update never reloads its own job, and skips recent checks
    make_release v3
    : > "$run_dir/calls"
    tools update --scheduled > "$run_dir/out" 2>&1 || fail "scheduled update failed"
    grep -q "fake provider v2" "$(install_dir)/bin/urnetwork" || fail "scheduled update ignored the recent check"
    URNETWORK_TOOLS_MODE=1 URNETWORK_PROVIDER_INSTALL="$(install_dir)" $sh_cmd "$script" auto-update > /dev/null 2>&1
    grep -v '^last_check=' "$(install_dir)/settings" > "$run_dir/s" && echo "last_check=0" >> "$run_dir/s" && cp "$run_dir/s" "$(install_dir)/settings"
    tools update --scheduled > "$run_dir/out" 2>&1 || fail "scheduled update failed: $(cat "$run_dir/out")"
    grep -q "fake provider v3" "$(install_dir)/bin/urnetwork" || fail "scheduled update did not update"
    grep -q "bootout gui/[0-9]*/com.urnetwork.provider.update" "$run_dir/calls" && fail "scheduled update reloaded its own job"
    loaded com.urnetwork.provider || fail "provider not restarted by the scheduled update"
    # an update heals a signed-in job that exited without registering
    mv "$HOME/.urnetwork/.provider.jwt" "$run_dir/pj"
    echo 0 > "$run_dir/launchd/com.urnetwork.provider.exited"
    tools update > "$run_dir/out" 2>&1 || fail "up-to-date update with a stuck job"
    grep -q "never registered" "$run_dir/out" || fail "update did not heal a stuck registration: $(cat "$run_dir/out")"
    [ -s "$HOME/.urnetwork/.provider.jwt" ] || fail "heal did not register"
    loaded com.urnetwork.provider || fail "heal left the provider stopped"
    rm -f "$run_dir/pj"
    # a scheduled update never installs a download it cannot check
    make_release v3b "" nodigest
    grep -v '^last_check=' "$(install_dir)/settings" > "$run_dir/s" && echo "last_check=0" >> "$run_dir/s" && cp "$run_dir/s" "$(install_dir)/settings"
    tools update --scheduled > "$run_dir/out" 2>&1 && fail "scheduled update without a checksum went ahead"
    grep -q "fake provider v3 " "$(install_dir)/bin/urnetwork" || fail "unchecked scheduled update replaced the binary"
    loaded com.urnetwork.provider || fail "refused scheduled update stopped the provider"

    # --- auto-update off and on
    tools auto-update off > /dev/null 2>&1 || fail "auto-update off"
    loaded com.urnetwork.provider.update && fail "auto-update off left the job loaded"
    [ -f "$HOME/Library/LaunchAgents/com.urnetwork.provider.update.plist" ] && fail "auto-update off left the plist"
    tools update --scheduled > "$run_dir/out" 2>&1
    grep -q "off" "$run_dir/out" || fail "scheduled update with auto-update off did something"
    tools auto-update --interval=monthly > /dev/null 2>&1 || fail "auto-update --interval=monthly"
    loaded com.urnetwork.provider.update && fail "--interval alone turned updates on"
    tools auto-update on > /dev/null 2>&1 || fail "auto-update on"
    loaded com.urnetwork.provider.update || fail "auto-update on did not load the job"
    grep -q "<key>Day</key>" "$HOME/Library/LaunchAgents/com.urnetwork.provider.update.plist" || fail "monthly schedule missing"
    tools auto-update sometimes > /dev/null 2>&1 && fail "invalid auto-update value accepted"

    # --- auto-start off parks the plist; start still works
    tools auto-start off > /dev/null 2>&1 || fail "auto-start off"
    [ -f "$HOME/Library/LaunchAgents/com.urnetwork.provider.plist" ] && fail "auto-start off left the login plist"
    [ -f "$(install_dir)/launchd/com.urnetwork.provider.plist" ] || fail "parked plist missing"
    loaded com.urnetwork.provider || fail "auto-start off stopped the running provider"
    tools restart > /dev/null 2>&1 || fail "restart with auto-start off"
    loaded com.urnetwork.provider || fail "restart did not start"
    tools auto-start on > /dev/null 2>&1 || fail "auto-start on"
    [ -f "$HOME/Library/LaunchAgents/com.urnetwork.provider.plist" ] || fail "auto-start on did not restore the login plist"

    # --- a damaged download or a binary that cannot run changes nothing
    make_release v4 "" badsum
    tools update > "$run_dir/out" 2>&1 && fail "bad checksum accepted"
    grep -q "damaged" "$run_dir/out" || fail "bad checksum message: $(cat "$run_dir/out")"
    grep -q "fake provider v3" "$(install_dir)/bin/urnetwork" || fail "bad download replaced the binary"
    loaded com.urnetwork.provider || fail "bad download stopped the provider"
    make_release v5 broken
    tools update > "$run_dir/out" 2>&1 && fail "broken binary accepted"
    grep -q "does not run" "$run_dir/out" || fail "broken binary message: $(cat "$run_dir/out")"
    grep -q "fake provider v3" "$(install_dir)/bin/urnetwork" || fail "broken binary installed"

    # --- option forms (the Linux installer rejected -t and -B)
    make_release v6
    tools reinstall -t 6 > "$run_dir/out" 2>&1 || fail "reinstall -t 6: $(cat "$run_dir/out")"
    [ "$(cat "$(install_dir)/.version")" = v6 ] || fail "-t did not pick v6"
    grep -q "auto-update off" "$run_dir/out" || fail "pinned reinstall does not mention auto-update"
    tools -t v6 reinstall > "$run_dir/out" 2>&1 || fail "option before command: $(cat "$run_dir/out")"
    tools reinstall --tag=v6 > /dev/null 2>&1 || fail "--tag=v6"
    tools update -t v6 > /dev/null 2>&1 && fail "update -t accepted"
    tools frobnicate > /dev/null 2>&1 && fail "unknown command accepted"
    $sh_cmd "$script" --bogus > /dev/null 2>&1 && fail "installer accepted --bogus"
    $sh_cmd "$script" --help | grep -q -- "--tag TAG" || fail "installer help"
    tools --help | grep -q "auto-update" || fail "tools help"

    # --- logs and service-run
    mkdir -p "$HOME/Library/Logs/urnetwork"
    echo "hello log" > "$HOME/Library/Logs/urnetwork/provider.log"
    tools logs --no-follow | grep -q "hello log" || fail "logs"
    rm -f "$HOME/.urnetwork/.provider.jwt"
    tools service-run > "$run_dir/out" 2>&1 || fail "service-run with identity"
    grep -q "providing v6 arm64" "$run_dir/out" || fail "service-run did not exec the provider: $(cat "$run_dir/out")"
    # the first start registers the device (released miner needs the flag);
    # later starts do not pass it
    grep -q -- "provide --allow-client-registration" "$run_dir/out" || fail "first start without --allow-client-registration: $(cat "$run_dir/out")"
    [ -s "$HOME/.urnetwork/.provider.jwt" ] || fail "first start did not register"
    tools service-run > "$run_dir/out" 2>&1 || fail "second service-run"
    grep -q -- "--allow-client-registration" "$run_dir/out" && fail "registered device still got --allow-client-registration"
    tools status 2>&1 | grep -q "Registered device:  yes" || fail "status does not show registration"
    rm -f "$HOME/.urnetwork/.provider.jwt"
    tools status 2>&1 | grep -q "Registered device:  not yet" || fail "status does not show missing registration"
    tools service-run > /dev/null 2>&1
    # a proxy slot's .provider-<hash>.jwt also means registered
    mv "$HOME/.urnetwork/.provider.jwt" "$HOME/.urnetwork/.provider-abc.jwt"
    tools service-run > "$run_dir/out" 2>&1
    grep -q -- "--allow-client-registration" "$run_dir/out" && fail "proxy-slot device got --allow-client-registration"
    mv "$HOME/.urnetwork/.provider-abc.jwt" "$HOME/.urnetwork/.provider.jwt"
    # a miner whose help lacks the flag gets plain provide
    cp "$(install_dir)/bin/urnetwork" "$run_dir/urnetwork.saved"
    sed 's/ \[--allow-client-registration\]//' "$run_dir/urnetwork.saved" > "$(install_dir)/bin/urnetwork"
    rm -f "$HOME/.urnetwork/.provider.jwt"
    tools service-run > "$run_dir/out" 2>&1
    grep -q -- "--allow-client-registration" "$run_dir/out" && fail "flag passed to a miner that lacks it"
    cp "$run_dir/urnetwork.saved" "$(install_dir)/bin/urnetwork"
    tools service-run > /dev/null 2>&1
    mv "$HOME/.urnetwork/jwt" "$run_dir/jwt"
    tools service-run > "$run_dir/out" 2>&1 || fail "service-run without identity must exit 0 (no restart loop)"
    grep -q "not signed in" "$run_dir/out" || fail "service-run without identity message"
    mv "$run_dir/jwt" "$HOME/.urnetwork/jwt"

    # --- a provider that keeps exiting is reported, with the right hint
    touch "$run_dir/launchd/com.urnetwork.provider.exited"
    tools status > "$run_dir/out" 2>&1
    grep -q "retries every minute" "$run_dir/out" || fail "status for an exiting provider: $(cat "$run_dir/out")"
    tools restart > /dev/null 2>&1

    # --- a big log is rotated in place, so launchd keeps writing to provider.log
    dd if=/dev/zero of="$HOME/Library/Logs/urnetwork/provider.log" bs=1048576 count=11 2> /dev/null
    tools service-run > /dev/null 2>&1
    [ -f "$HOME/Library/Logs/urnetwork/provider.log.1" ] || fail "log not rotated"
    [ "$(wc -c < "$HOME/Library/Logs/urnetwork/provider.log")" -lt 1000 ] || fail "rotated log not truncated"

    # --- an update that fails after stopping the provider starts it again
    make_release v7
    mkdir -p "$(install_dir)/bin/urnetwork.new/x"
    tools update > "$run_dir/out" 2>&1 && fail "update over a blocked path succeeded"
    loaded com.urnetwork.provider || fail "failed update left the provider stopped: $(cat "$run_dir/out")"
    rm -rf "$(install_dir)/bin/urnetwork.new"
    tools update > /dev/null 2>&1 || fail "update after clearing the block"
    grep -q "fake provider v7" "$(install_dir)/bin/urnetwork" || fail "v7 not installed"

    tools update --no-start > /dev/null 2>&1 && fail "update --no-start accepted"
    $sh_cmd "$script" -v > /dev/null 2>&1 && fail "installer -v accepted"

    # --- dry run changes nothing
    find "$HOME" -exec ls -ld {} + 2>/dev/null | sort > "$run_dir/before"
    cp -r "$run_dir/launchd" "$run_dir/launchd.before"
    tools --dry-run reinstall > "$run_dir/out" 2>&1 || fail "dry-run reinstall: $(cat "$run_dir/out")"
    tools --dry-run uninstall --remove-identity -y > "$run_dir/out" 2>&1 || fail "dry-run uninstall: $(cat "$run_dir/out")"
    find "$HOME" -exec ls -ld {} + 2>/dev/null | sort > "$run_dir/after"
    cmp -s "$run_dir/before" "$run_dir/after" || fail "dry run changed files: $(diff "$run_dir/before" "$run_dir/after" | head -5)"
    diff -r "$run_dir/launchd" "$run_dir/launchd.before" > /dev/null || fail "dry run changed launchd"
    grep -q "\[dry-run\]" "$run_dir/out" || fail "dry run printed nothing"

    # --- uninstall keeps the identity unless asked
    echo "# mine" >> "$HOME/.zshrc"
    tools uninstall > "$run_dir/out" 2>&1 || fail "uninstall: $(cat "$run_dir/out")"
    [ -d "$(install_dir)" ] && fail "install folder left"
    loaded com.urnetwork.provider && fail "provider left loaded"
    loaded com.urnetwork.provider.update && fail "update job left loaded"
    ls "$HOME/Library/LaunchAgents/"com.urnetwork* > /dev/null 2>&1 && fail "plists left"
    [ -s "$HOME/.urnetwork/jwt" ] || fail "uninstall deleted the identity (Linux bug)"
    grep -q "urnetwork-provider" "$HOME/.zshrc" && fail "PATH lines left"
    grep -q "# mine" "$HOME/.zshrc" || fail "uninstall removed the user's own lines"
    $sh_cmd "$script" -y > /dev/null 2>&1 || fail "install after uninstall"
    loaded com.urnetwork.provider || fail "install with an existing identity did not start the provider"
    tools uninstall --remove-identity < /dev/null > "$run_dir/out" 2>&1
    if (exec < /dev/tty) 2>/dev/null; then :; else
        [ -d "$(install_dir)" ] || fail "--remove-identity without a terminal or -y went ahead"
    fi
    tools uninstall --remove-identity -y > /dev/null 2>&1 || fail "uninstall --remove-identity -y"
    [ -d "$HOME/.urnetwork" ] && fail "--remove-identity kept the identity"

    # --- --auth-code signs in and starts mining without a prompt
    $sh_cmd "$script" -y --auth-code=abc123 > "$run_dir/out" 2>&1 || fail "install --auth-code: $(cat "$run_dir/out")"
    grep -q "abc123" "$HOME/.urnetwork/jwt" 2>/dev/null || fail "--auth-code was not passed to auth"
    loaded com.urnetwork.provider || fail "--auth-code install did not start the provider"
    tools reinstall --auth-code x > /dev/null 2>&1 && fail "reinstall --auth-code accepted"
    tools uninstall -y > /dev/null 2>&1

    # --- a per-platform tarball wins over the all-platform one
    make_release v8
    mkdir -p "$run_dir/pp"
    printf '#!/bin/sh\n# per-platform v8\n' > "$run_dir/pp/provider"
    chmod 755 "$run_dir/pp/provider"
    tar -czf "$run_dir/fixtures/urnetwork-provider-v8-darwin-arm64.tar.gz" -C "$run_dir/pp" provider
    pp_sum="$(shasum -a 256 "$run_dir/fixtures/urnetwork-provider-v8-darwin-arm64.tar.gz" | cut -d' ' -f1)"
    sed "s|\"assets\": \[|\"assets\": [{\"name\": \"urnetwork-provider-v8-linux-arm64.tar.gz\", \"browser_download_url\": \"https://dl.test/nope\"}, {\"name\": \"urnetwork-provider-v8-darwin-arm64.tar.gz\", \"browser_download_url\": \"https://dl.test/urnetwork-provider-v8-darwin-arm64.tar.gz\", \"digest\": \"sha256:$pp_sum\"},|" \
        "$run_dir/fixtures/tag-v8.json" > "$run_dir/fixtures/latest.json"
    $sh_cmd "$script" -y > "$run_dir/out" 2>&1 || fail "per-platform install: $(cat "$run_dir/out")"
    grep -q "per-platform v8" "$(install_dir)/bin/urnetwork" || fail "per-platform tarball not preferred"
    MOCK_ARCH=x86_64 tools reinstall > /dev/null 2>&1 || fail "Intel reinstall with only an arm64 per-platform file"
    grep -q "fake provider v8 amd64" "$(install_dir)/bin/urnetwork" || fail "Intel did not fall back to the all-platform tarball"
    tools uninstall -y > /dev/null 2>&1
    cp "$run_dir/fixtures/tag-v8.json" "$run_dir/fixtures/latest.json"

    # --- curl | sh: $0 is the shell, so urnet-tools comes from the published URL
    cp "$script" "$run_dir/fixtures/tools.sh"
    (cd "$run_dir" && $sh_cmd -s -- -y < "$script") > "$run_dir/out" 2>&1 || fail "piped install: $(cat "$run_dir/out")"
    cmp -s "$(install_dir)/bin/urnet-tools" "$script" && fail "piped install copied the script without the mode line"
    grep -q "^provider_label=" "$(install_dir)/bin/urnet-tools" || fail "piped install has no urnet-tools"
    tools status 2>&1 | grep -q "Installed version:  v8" || fail "status after piped install"
    tools uninstall -y > /dev/null 2>&1
    rm -f "$run_dir/fixtures/tools.sh"
    rm -f "$HOME/.zshrc"
    (cd "$run_dir" && $sh_cmd -s -- -y < "$script") > "$run_dir/out" 2>&1 && fail "piped install without a published script went ahead"
    [ -e "$(install_dir)" ] && fail "failed piped install left files behind"

    # --- unsafe install paths and root are refused; a shared folder keeps other files
    $sh_cmd "$script" -y -B -i "$HOME" > "$run_dir/out" 2>&1 && fail "install into HOME accepted"
    [ -d "$HOME/bin" ] && fail "install into HOME created files"
    $sh_cmd "$script" -y -B -i "$run_dir/we\$ird" > /dev/null 2>&1 && fail "install path with a dollar sign accepted"
    MOCK_UID=0 $sh_cmd "$script" -y > "$run_dir/out" 2>&1 && fail "root install accepted"
    grep -q "without sudo" "$run_dir/out" || fail "root refusal message"
    mkdir -p "$run_dir/shared"
    echo keep > "$run_dir/shared/mine.txt"
    $sh_cmd "$script" -y -i "$run_dir/shared" > /dev/null 2>&1 || fail "install into a shared folder"
    grep -q "URNETWORK_PROVIDER_INSTALL=\"$run_dir/shared\"" "$HOME/.zshrc" || fail "PATH block not moved to the new install path"
    [ "$(grep -c "urnetwork-provider start" "$HOME/.zshrc")" = 1 ] || fail "PATH block duplicated on path change"
    URNETWORK_PROVIDER_INSTALL="$run_dir/shared" $sh_cmd "$run_dir/shared/bin/urnet-tools" uninstall -y > /dev/null 2>&1 || fail "uninstall shared"
    [ -f "$run_dir/shared/mine.txt" ] || fail "uninstall deleted a file it did not install"
    [ -e "$run_dir/shared/bin" ] && fail "uninstall left its bin folder"

    # --- bash with only ~/.profile: no new ~/.bash_profile that would hide it
    rm -f "$HOME/.zshrc" "$HOME/.bash_profile"
    echo "# my profile" > "$HOME/.profile"
    SHELL=/bin/bash $sh_cmd "$script" -y > /dev/null 2>&1 || fail "install for a .profile user"
    [ -f "$HOME/.bash_profile" ] && fail "created ~/.bash_profile over an existing ~/.profile"
    grep -q "urnetwork-provider start" "$HOME/.profile" || fail "PATH not added to ~/.profile"
    tools uninstall -y > /dev/null 2>&1
    grep -q "urnetwork-provider" "$HOME/.profile" && fail "PATH left in ~/.profile"
    rm -f "$HOME/.profile"

    # --- -B, bash users, Intel, Rosetta, SSH sessions, other systems
    SHELL=/bin/bash $sh_cmd "$script" -y -B > /dev/null 2>&1 || fail "install -B"
    grep -q "urnetwork-provider" "$HOME/.bash_profile" 2>/dev/null && fail "-B modified ~/.bash_profile"
    tools uninstall -y -B > /dev/null 2>&1
    rm -f "$HOME/.zshrc"
    SHELL=/bin/bash $sh_cmd "$script" -y > /dev/null 2>&1 || fail "install for bash"
    grep -q "urnetwork-provider start" "$HOME/.bash_profile" || fail "bash user got no ~/.bash_profile PATH"
    tools uninstall -y > /dev/null 2>&1
    MOCK_ARCH=x86_64 $sh_cmd "$script" -y > /dev/null 2>&1 || fail "Intel install"
    grep -q "fake provider v8 amd64" "$(install_dir)/bin/urnetwork" || fail "Intel got the wrong binary"
    tools uninstall -y > /dev/null 2>&1
    MOCK_ARCH=x86_64 MOCK_TRANSLATED=1 $sh_cmd "$script" -y > /dev/null 2>&1 || fail "Rosetta install"
    grep -q "fake provider v8 arm64" "$(install_dir)/bin/urnetwork" || fail "Rosetta shell got the Intel binary"
    tools uninstall -y > /dev/null 2>&1
    MOCK_GUI=0 $sh_cmd "$script" -y > /dev/null 2>&1 || fail "SSH install"
    grep -q "^user/" "$run_dir/launchd/com.urnetwork.provider.update.domain" || fail "SSH install did not use the user domain"
    # a screen login later: the job stays in user/, and every command still finds it
    tools uninstall -y > /dev/null 2>&1 || fail "uninstall after SSH install"
    loaded com.urnetwork.provider.update && fail "uninstall missed the job in the user domain"
    MOCK_ARCH=ppc $sh_cmd "$script" -y > "$run_dir/out" 2>&1 && fail "ppc accepted"
    MOCK_UNAME=Linux $sh_cmd "$script" -y > "$run_dir/out" 2>&1 && fail "Linux accepted"
    grep -q "Provider_Install_Linux.sh" "$run_dir/out" || fail "Linux refusal does not point to the Linux installer"

    rm -rf "$run_dir"
}

for sh_name in $shells; do
    sh_cmd="$sh_name"
    run_tests
done

if [ "$failures" -ne 0 ]; then
    echo "Provider_Install_macOS: $failures failure(s)" >&2
    exit 1
fi
echo "Provider_Install_macOS: OK ($shells)"
