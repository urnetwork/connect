#!/bin/sh

me="$0"
SCRIPT_VERSION="1.0.0"

if [ "$me" = "sh" ] || [ "$me" = "bash" ] || [ "$me" = "zsh" ]; then
    me="urnetwork-installer"
fi

show_help() {
    echo "Usage: "
    echo "  $me [options]"
    echo ""
    echo "Installs Urnetwork locally for the current user."
    echo ""
    echo "Options:"
    echo "  -h, --help              Show this help and exit"
    echo "  -v, --version           Show the version of this script"
    echo "  -t, --tag=[TAG]         Install a specific version/tag of Urnetwork"
    echo "  -d, --dest=[PATH]       Installation destination"
    echo "  -B, --no-modify-bashrc  Do not modify ~/.bashrc"
    echo ""
    echo "Refer to the online documentation for more help."
}

show_version() {
    echo "Urnetwork installer $SCRIPT_VERSION"
}

require_arg() {
    if [ -z "$2" ]; then
	echo "$me: Option '$1' requires an argument" >&1
	exit 1
    fi
}

tag="latest"
install_path="$HOME/.local/share/urnetwork-provider"
no_modify_bashrc=0

while [ $# -gt 0 ]; do
    case "$1" in
	-h|--help)
	    show_help
	    exit 0
	    ;;

	-v|--version)
	    show_version
	    exit 0
	    ;;

	-t|--tag)
	    require_arg "$1" "$2"
	    tag="tags/$2"
	    shift 2
	    ;;

	-d|--dest)
	    require_arg "$1" "$2"
	    install_path="$2"
	    shift 2
	    ;;

	-B|--no-modify-bashrc)
	    no_modify_bashrc=1
	    shift
	    ;;
	
	--)
	    shift
	    break
	    ;;
	
	-*)
	    echo "$me: Invalid option '$1'" >&1
	    exit 1
	    ;;
	
	*)
	    break
	    ;;
    esac
done

if [ $# -gt 0 ]; then
    echo "$me: Invalid argument '$1'" >&1
    exit 1
fi

check_command() {
    if command -v "$1" > /dev/null; then
	return 1
    else
	echo "$me: '$1' not found. Please install it, and then re-run this installer script." >&2
	exit 1
    fi
}

for cmd in tar curl; do
    check_command "$cmd"
done

echo "$me: Installing Urnetwork version $tag"

api_url="https://api.github.com/repos/urnetwork/build/releases/$tag"
release="$(curl -fSsL "$api_url")"
dl_url=""

if command -v python3 > /dev/null; then
    dl_url="$(echo "$release" | tr -d '\000-\037' | python3 -c 'import sys, json; data = json.load(sys.stdin); assets = data["assets"]; asset = next(a for a in assets if a["name"].startswith("urnetwork-provider")); print(asset["browser_download_url"] if asset else "")')"
elif command -v jq > /dev/null; then
    asset="$(echo "$release" | tr -d '\000-\037' | jq -r '.assets[] | select(.name | startswith("urnetwork-provider-"))')"

    if [ -z "$asset" ]; then
	echo "$me: Could not find a suitable release asset for tag '$tag'" >&2
	exit 1
    fi

    dl_url="$(echo "$asset" | jq -r '.browser_download_url')"
else
    echo "$me: Neither python3 nor jq is available" >&2
    exit 1
fi

if [ -z "$dl_url" ]; then
    echo "$me: No download URL found for tag '$tag'" >&2
    exit 1
fi

echo "$me: Downloading $dl_url"

tmpdir="$(mktemp -d)"
urnetwork_tar_gz="$tmpdir/urnetwork.tar.gz"

trap 'rm -r "$tmpdir"' EXIT 
trap 'exit 1' INT TERM

if ! curl --progress-bar -L "$dl_url" -o "$urnetwork_tar_gz"; then
    echo "$me: Failed to download $dl_url" >&2
    exit 1
fi

cd "$tmpdir" || exit 1

if ! tar -xf "$urnetwork_tar_gz" 2>/dev/null; then
    echo "$me: Failed to extract $urnetwork_tar_gz" >&2
    exit 1
fi

arch="$(arch)"

case "$arch" in
    i386|i686)
	arch=386
	;;

    x86_64)
	arch=amd64
	;;

    aarch64)
	arch=arm64
	;;
esac

bindir="$tmpdir/linux/$arch"

if [ ! -d "$bindir" ]; then
    echo "$me: The architecture of this system ($arch/$(arch)) is not supported" >&2
    echo "$me: Aborting installation" >&2
    exit 1
fi

bin="$bindir/provider"

if [ ! -f "$bin" ]; then
    echo "$me: Provider binary not found" >&2
    echo "$me: This indicates an issue with the package that was downloaded" >&2
    exit 1
fi

if [ ! -d "$install_path" ]; then
    echo "$me: Creating directory '$install_path'"

    if ! mkdir -p "$install_path"; then
	echo "$me: Failed to create directory '$install_path'" >&2
	exit 1
    fi
fi

if ! mkdir -p "$install_path/bin"; then
    echo "$me: Failed to create directory '$install_path/bin'" >&2
    exit 1
fi

if [ -f "$install_path/bin/urnetwork" ]; then
    echo "$me: Found existing installation"
    no_modify_bashrc=1
fi

cp "$bin" "$install_path/bin/urnetwork" || exit 1
chmod 755 "$install_path/bin/urnetwork" || exit 1

if command -v systemctl > /dev/null; then
    systemd_userdir="$HOME/.config/systemd/user"
    systemd_service="$systemd_userdir/urnetwork.service"
    echo "$me: Installing systemd user unit in $systemd_service"

    mkdir -p "$systemd_userdir"
    
    cat > "$systemd_service" <<EOF
[Unit]
Description=Urnetwork Provider

[Service]
ExecStart=$install_path/bin/urnetwork provide
Restart=no

[Install]
WantedBy=default.target
EOF
    if ! systemctl --user enable urnetwork.service; then
	echo "$me: Could not enable the newly installed systemd service" >&2
	exit 1
    fi
fi

if [ "$no_modify_bashrc" -eq 0 ]; then
echo "$me: Adding '$install_path/bin' to ~/.bashrc"

cat >> $HOME/.bashrc <<EOF

# == urnetwork-provider start
export URNETWORK_PROVIDER_INSTALL="$install_path"
export PATH="\$PATH:\$URNETWORK_PROVIDER_INSTALL/bin"
# == urnetwork-provider end
EOF
fi

echo "$me: Installation complete"
echo ""
printf "Reload shell:   \e[1msource ~/.bashrc\e[0m           # or restart your terminal\n"
printf "First run:      \e[1murnetwork auth\e[0m             # Auth code can be found at <https://ur.io>\n"
printf "Start:          \e[1murnetwork provide\e[0m          # Foreground\n"

if command -v systemctl > /dev/null; then
    printf "Start service:  \e[1msystemctl --user start urnetwork\e[0m\n"
    printf "Disable:        \e[1msystemctl --user disable urnetwork\e[0m\n"
    echo ""
    printf "\e[1mRefer to <https://docs.ur.io/provider#linux-and-macos> for more detailed instructions.\e[0m\n"
fi
