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
    echo "  --purge                 Also delete ~/.urnetwork (this device's miner identity)"
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

	--purge)
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

if [ ! -d "$install_path" ]; then
    echo "$me: Path '$install_path' does not exist - are you sure you have had Urnetwork installed?" >&2
    exit 1
fi

unit_dir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"

if command -v systemctl > /dev/null; then
    for unit in urnetwork.service urnetwork-update.timer urnetwork-update.service; do
	if [ -f "$unit_dir/$unit" ]; then
	    echo "$me: Removing systemd unit - $unit"
	    systemctl --user disable --now "$unit" > /dev/null 2>&1
	    rm -f "$unit_dir/$unit"
	fi
    done

    systemctl --user daemon-reload > /dev/null 2>&1
fi

echo "$me: Removing '$install_path'"
rm -r "$install_path"

if [ "$purge" -eq 1 ]; then
    echo "$me: Removing '$HOME/.urnetwork'"
    rm -rf "$HOME/.urnetwork"
elif [ -d "$HOME/.urnetwork" ]; then
    echo "$me: Kept your miner identity in '$HOME/.urnetwork' (add --purge to delete it)"
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
