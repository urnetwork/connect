#!/bin/sh

me="$0"
SCRIPT_VERSION="1.1.0"

if [ "$me" = "sh" ] || [ "$me" = "bash" ] || [ "$me" = "zsh" ]; then
    me="urnetwork-uninstaller"
fi

show_help() {
    echo "Usage: "
    echo "  $me [options]"
    echo ""
    echo "Uninstalls Urnetwork for the current user."
    echo ""
    echo "Options:"
    echo "  -h, --help              Show this help and exit"
    echo "  -v, --version           Show the version of this script"
    echo "  -d, --dest=[PATH]       Installation destination"
    echo "  -B, --no-modify-bashrc  Do not modify ~/.bashrc"
    echo "  --remove-identity       Also delete ~/.urnetwork (this device's miner identity)"
    echo ""
    echo "Refer to the online documentation for more help."
}

show_version() {
    echo "Urnetwork uninstaller $SCRIPT_VERSION"
}

require_arg() {
    if [ -z "$2" ]; then
	echo "$me: Option '$1' requires an argument" >&1
	exit 1
    fi
}

install_path="$HOME/.local/share/urnetwork-provider"
no_modify_bashrc=0
purge=0

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

	-d|--dest)
	    require_arg "$1" "$2"
	    install_path="$2"
	    shift 2
	    ;;

	-B|--no-modify-bashrc|--no_modify_bashrc)
	    no_modify_bashrc=1
	    shift
	    ;;

	--dest=*)
	    install_path="${1#*=}"
	    require_arg --dest "$install_path"
	    shift
	    ;;

	--remove-identity|--purge)
	    purge=1
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

unit_dir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
found=0

if [ -d "$install_path" ]; then
    case "$install_path" in
	/|"$HOME"|"$HOME/"|"$HOME/.urnetwork"|"$HOME/.urnetwork/")
	    echo "$me: Refusing to remove '$install_path'" >&2
	    exit 1
	    ;;
    esac

    if [ ! -f "$install_path/.version" ] && [ ! -f "$install_path/bin/urnetwork" ]; then
	echo "$me: '$install_path' doesn't look like a URnetwork installation; not removing it" >&2
	exit 1
    fi
fi

# `su - user' and `sudo -iu user' shells often lack XDG_RUNTIME_DIR
if [ -z "$XDG_RUNTIME_DIR" ] && [ -S "/run/user/$(id -u)/bus" ]; then
    XDG_RUNTIME_DIR="/run/user/$(id -u)"
    export XDG_RUNTIME_DIR
fi

if [ -f "$unit_dir/urnetwork.service" ] || [ -f "$unit_dir/urnetwork-update.timer" ] || [ -f "$unit_dir/urnetwork-update.service" ]; then
    found=1

    if command -v systemctl > /dev/null; then
	if ! systemctl --user show-environment > /dev/null 2>&1; then
	    echo "$me: There is no systemd user session for $(id -un), so the miner can't be stopped from here" >&2
	    echo "$me: Log in as $(id -un) directly (SSH or console) and run this again. Nothing was removed" >&2
	    exit 1
	fi

	echo "$me: Stopping and removing the systemd units"

	# one by one: disabling a unit whose file is gone fails
	for unit in urnetwork.service urnetwork-update.timer; do
	    systemctl --user disable --now "$unit" > /dev/null 2>&1
	done

	systemctl --user stop urnetwork-update.service > /dev/null 2>&1

	if systemctl --user is-active --quiet urnetwork.service; then
	    echo "$me: Could not stop urnetwork.service; nothing else was removed" >&2
	    exit 1
	fi
    fi

    rm -f "$unit_dir/urnetwork.service" "$unit_dir/urnetwork-update.timer" "$unit_dir/urnetwork-update.service"

    if command -v systemctl > /dev/null; then
	systemctl --user daemon-reload > /dev/null 2>&1
	systemctl --user reset-failed urnetwork.service urnetwork-update.service > /dev/null 2>&1
    fi
fi

if [ -d "$install_path" ]; then
    found=1
    echo "$me: Removing '$install_path'"
    rm -r "$install_path"
fi

if [ "$found" -eq 0 ]; then
    echo "$me: Path '$install_path' does not exist - are you sure you have had Urnetwork installed?" >&2
    exit 1
fi

if [ "$purge" -eq 1 ]; then
    echo "$me: Removing '$HOME/.urnetwork'"
    rm -rf "$HOME/.urnetwork"
elif [ -d "$HOME/.urnetwork" ]; then
    echo "$me: Kept your miner identity in '$HOME/.urnetwork' (add --remove-identity to delete it)"
fi

if [ "$no_modify_bashrc" -eq 0 ]; then
    if ! command -v awk > /dev/null; then
	echo "$me: warning: awk not found, cannot modify ~/.bashrc" >&2
    else
	for rc in "$HOME/.bashrc" "$HOME/.zshrc"; do
	    if [ -f "$rc" ] && grep -q '# == urnetwork-provider start' "$rc"; then
		echo "$me: Removing PATH exports from $rc"
		cp "$rc" "$rc.backup.old"
		awk '/# == urnetwork-provider start/ { pr=1 } pr == 0 { print } /# == urnetwork-provider end/ { pr=0 }' "$rc" > "$rc.new" &&
		    cat "$rc.new" > "$rc"
		rm -f "$rc.new"
	    fi
	done
    fi
fi

echo "$me: Uninstallation successful"
