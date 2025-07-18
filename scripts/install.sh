#!/bin/bash
#
# urnetwork-provider - Unified Installer, Uninstaller, and Manager
#
# This script handles the complete lifecycle of the urnetwork provider binary
# on a user's system. It supports installation, upgrades from previous
# versions, updates, and complete uninstallation.
#
# Web: https://ur.io/
# GitHub: https://github.com/urnetwork/connect

set -e # Exit immediately if a command exits with a non-zero status.
set -o pipefail # The return value of a pipeline is the status of the last command to exit with a non-zero status.

# --- Script Configuration and Variables ---
SCRIPT_VERSION="2.0.0"
ME="urnetwork-manager"
GITHUB_REPO="urnetwork/build"

# New standardized installation directory
URNETWORK_HOME="${URNETWORK_HOME:-$HOME/.urnetwork}"
BIN_DIR="$URNETWORK_HOME/bin"
LOGS_DIR="$URNETWORK_HOME/logs"
MANAGER_SCRIPT_PATH="$BIN_DIR/urnetwork-manager"
PROVIDER_BINARY_PATH="$BIN_DIR/urnetwork"
VERSION_FILE="$URNETWORK_HOME/version"
LAST_UPDATE_CHECK_FILE="$URNETWORK_HOME/last_update_check"

# Path for legacy installations (for migration)
OLD_INSTALL_PATH="$HOME/.local/share/urnetwork-provider"
OLD_BASHRC_MARKER="# == urnetwork-provider start"

# --- Helper Functions ---

# Colored logging functions for better UX
log_info() {
    printf "\e[1;34m%s\e[0m\n" "$1"
}
log_success() {
    printf "\e[1;32m%s\e[0m\n" "$1"
}
log_warn() {
    printf "\e[1;33m%s\e[0m\n" "$1"
}
log_error() {
    printf "\e[1;31mError: %s\e[0m\n" "$1" >&2
}

# Ensures a command is available on the system
check_dep() {
    if ! command -v "$1" &>/dev/null; then
        log_error "'$1' command not found. Please install it to continue."
        exit 1
    fi
}

# Ensures either jq or python3 is available for JSON parsing
check_json_parser() {
    if command -v "jq" &>/dev/null; then
        JSON_PARSER="jq"
    elif command -v "python3" &>/dev/null; then
        JSON_PARSER="python3"
    else
        log_error "Neither 'jq' nor 'python3' found. Please install one to parse GitHub API responses."
        exit 1
    fi
}

# Detects the system architecture and maps it to Go-style arch names
detect_arch() {
    local arch
    arch=$(uname -m)
    case "$arch" in
        i386|i686) arch="386" ;;
        x86_64) arch="amd64" ;;
        aarch64|armv8l) arch="arm64" ;;
        armv7l) arch="arm" ;;
        *)
            log_error "Unsupported architecture: $arch. Cannot install."
            exit 1
            ;;
    esac
    echo "$arch"
}

# Fetches the download URL for a specific release tag from GitHub
get_download_url() {
    local tag="$1"
    local api_url="https://api.github.com/repos/$GITHUB_REPO/releases"
    
    if [[ "$tag" == "latest" ]]; then
        api_url+="/latest"
    else
        api_url+="/tags/$tag"
    fi

    log_info "Fetching release information from GitHub..."
    local response
    response=$(curl -fSsL "$api_url")
    if [ -z "$response" ]; then
        log_error "Failed to fetch release info for tag '$tag'. Check your connection or the tag name."
        exit 1
    fi

    local dl_url
    if [[ "$JSON_PARSER" == "jq" ]]; then
        dl_url=$(echo "$response" | jq -r '.assets[] | select(.name | startswith("urnetwork-provider-")) | .browser_download_url')
    else
        dl_url=$(echo "$response" | python3 -c 'import sys, json; d = json.load(sys.stdin); print(next(a["browser_download_url"] for a in d["assets"] if a["name"].startswith("urnetwork-provider-")))')
    fi

    if [ -z "$dl_url" ]; then
        log_error "Could not find a download URL for tag '$tag'."
        exit 1
    fi
    echo "$dl_url"
}

# Updates shell profiles (.bashrc, .zshrc, .profile) to include our bin directory
update_shell_profiles() {
    log_info "Updating shell configuration files..."
    local profile_files=("$HOME/.bashrc" "$HOME/.zshrc" "$HOME/.profile")
    local new_path_export_block
    new_path_export_block=$(cat <<EOF

# === urnetwork-manager start ===
export URNETWORK_HOME="$URNETWORK_HOME"
export PATH="\$URNETWORK_HOME/bin:\$PATH"
# === urnetwork-manager end ===
EOF
)

    for rc_file in "${profile_files[@]}"; do
        if [ -f "$rc_file" ]; then
            # Remove old blocks first (from legacy and current versions)
            sed -i.urnetwork.bak '/# == urnetwork-provider start/,/# == urnetwork-provider end/d' "$rc_file"
            sed -i.urnetwork.bak '/# === urnetwork-manager start ===/,/# === urnetwork-manager end ===/d' "$rc_file"
            
            # Add the new block
            echo "$new_path_export_block" >> "$rc_file"
            rm -f "${rc_file}.urnetwork.bak"
            log_success "  -> Updated $rc_file"
        fi
    done
}

# Removes PATH configuration from shell profiles
cleanup_shell_profiles() {
    log_info "Cleaning up shell configuration files..."
    local profile_files=("$HOME/.bashrc" "$HOME/.zshrc" "$HOME/.profile")
    for rc_file in "${profile_files[@]}"; do
        if [ -f "$rc_file" ]; then
            sed -i.urnetwork.bak '/# == urnetwork-provider start/,/# == urnetwork-provider end/d' "$rc_file"
            sed -i.urnetwork.bak '/# === urnetwork-manager start ===/,/# === urnetwork-manager end ===/d' "$rc_file"
            rm -f "${rc_file}.urnetwork.bak"
            log_success "  -> Cleaned $rc_file"
        fi
    done
}

# --- Core Logic Functions ---

do_install() {
    local tag="${1:-latest}"
    local is_upgrade=false

    log_info "Starting Urnetwork Provider installation (v$SCRIPT_VERSION)..."
    check_dep "curl"
    check_dep "tar"
    check_json_parser

    # --- Backward Compatibility: Detect and handle old installations ---
    if [ -d "$OLD_INSTALL_PATH" ]; then
        is_upgrade=true
        log_warn "Legacy installation found at '$OLD_INSTALL_PATH'. Upgrading to new manager."
    elif [ -f "$PROVIDER_BINARY_PATH" ]; then
        log_info "Urnetwork provider is already installed. Checking for updates..."
        do_update
        exit 0
    fi
    
    local arch
    arch=$(detect_arch)
    log_info "Detected Architecture: $arch"

    # --- Download and Extract ---
    local download_url
    download_url=$(get_download_url "$tag")
    local release_tag
    release_tag=$(echo "$download_url" | grep -oP '(?<=/download/)[^/]+')

    log_info "Downloading Urnetwork Provider ($release_tag) from: $download_url"
    local tmpdir
    tmpdir=$(mktemp -d)
    trap 'rm -rf "$tmpdir"' EXIT

    if ! curl --progress-bar -L "$download_url" -o "$tmpdir/urnetwork.tar.gz"; then
        log_error "Download failed."
        exit 1
    fi
    
    log_info "Extracting archive..."
    if ! tar -xzf "$tmpdir/urnetwork.tar.gz" -C "$tmpdir"; then
        log_error "Extraction failed."
        exit 1
    fi

    # --- Place Binaries ---
    mkdir -p "$BIN_DIR" "$LOGS_DIR"
    local binary_source_path="$tmpdir/linux/$arch/provider"
    
    if [ ! -f "$binary_source_path" ]; then
        log_error "Provider binary not found in the downloaded archive for architecture '$arch'."
        exit 1
    fi
    
    log_info "Installing binary to $PROVIDER_BINARY_PATH"
    if [ -f "$PROVIDER_BINARY_PATH" ]; then
        # If provider is running, stop it before replacing the binary
        if command -v systemctl &>/dev/null && systemctl --user is-active --quiet urnetwork.service; then
            log_info "Stopping existing urnetwork service before upgrade..."
            systemctl --user stop urnetwork.service
        fi
    fi
    
    cp "$binary_source_path" "$PROVIDER_BINARY_PATH"
    chmod 755 "$PROVIDER_BINARY_PATH"
    echo "$release_tag" > "$VERSION_FILE"

    # --- Setup PATH ---
    update_shell_profiles

    # --- Setup systemd Service ---
    if command -v systemctl &>/dev/null; then
        log_info "Setting up systemd user services..."
        local systemd_dir="$HOME/.config/systemd/user"
        mkdir -p "$systemd_dir"

        # Main provider service
        cat > "$systemd_dir/urnetwork.service" <<EOF
[Unit]
Description=Urnetwork Provider Service
After=network.target

[Service]
ExecStart=$PROVIDER_BINARY_PATH provide
Restart=on-failure
RestartSec=5
StandardOutput=append:$LOGS_DIR/provider.log
StandardError=append:$LOGS_DIR/provider.err

[Install]
WantedBy=default.target
EOF

        # Auto-update timer
        cat > "$systemd_dir/urnetwork-update.timer" <<EOF
[Unit]
Description=Weekly check for urnetwork-provider updates

[Timer]
OnCalendar=weekly
Persistent=true

[Install]
WantedBy=timers.target
EOF

        cat > "$systemd_dir/urnetwork-update.service" <<EOF
[Unit]
Description=Urnetwork Provider Update Check

[Service]
Type=oneshot
ExecStart=$MANAGER_SCRIPT_PATH --update --quiet
EOF
        log_info "Reloading systemd daemon and enabling services..."
        systemctl --user daemon-reload
        systemctl --user enable --now urnetwork.service
        systemctl --user enable --now urnetwork-update.timer
    else
        log_warn "systemd not found. You will need to manage the provider process manually."
    fi

    # --- Install Manager Script ---
    log_info "Installing manager script to $MANAGER_SCRIPT_PATH"
    # shellcheck disable=SC2154 # me is defined but shellcheck misses it in some contexts
    if [[ "$0" != "$MANAGER_SCRIPT_PATH" ]]; then
        cp "$0" "$MANAGER_SCRIPT_PATH"
        chmod 755 "$MANAGER_SCRIPT_PATH"
    fi

    # --- Cleanup Legacy Installation ---
    if [ "$is_upgrade" = true ]; then
        log_info "Cleaning up legacy installation..."
        rm -rf "$OLD_INSTALL_PATH"
    fi
    
    # --- Final Instructions ---
    log_success "\nInstallation successful!"
    log_success "Urnetwork Provider version $release_tag has been installed."
    echo ""
    log_info "Please run the following commands:"
    echo "  1. Reload your shell: source ~/.bashrc (or .zshrc/.profile, or restart terminal)"
    echo "  2. Authenticate:        urnetwork auth"
    echo "  3. Check status:        systemctl --user status urnetwork"
    echo ""
    log_info "The provider is now running in the background via systemd."
    log_info "Manage it with: urnetwork-manager --[update|uninstall|version]"
}

do_uninstall() {
    log_info "Starting uninstallation of Urnetwork Provider..."
    
    if [ ! -d "$URNETWORK_HOME" ]; then
        log_warn "Urnetwork installation not found at $URNETWORK_HOME."
        # Still attempt to clean up legacy paths just in case
        cleanup_shell_profiles
        log_warn "If you had a legacy install, it has been cleaned from your shell profile."
        exit 0
    fi
    
    # --- Stop and Disable systemd Services ---
    if command -v systemctl &>/dev/null; then
        log_info "Disabling and removing systemd services..."
        systemctl --user disable --now urnetwork.service &>/dev/null || true
        systemctl --user disable --now urnetwork-update.timer &>/dev/null || true
        rm -f "$HOME/.config/systemd/user/urnetwork.service"
        rm -f "$HOME/.config/systemd/user/urnetwork-update.service"
        rm -f "$HOME/.config/systemd/user/urnetwork-update.timer"
        systemctl --user daemon-reload
    fi
    
    # --- Clean Shell Profiles ---
    cleanup_shell_profiles
    
    # --- Remove Application Files ---
    log_info "Removing all application files from $URNETWORK_HOME..."
    rm -rf "$URNETWORK_HOME"
    
    log_info "Checking for user data directory..."
    if [ -d "$HOME/.urnetwork" ]; then
        read -p "$(echo -e "\e[1;33mDo you want to remove the user data directory (~/.urnetwork)? This will delete your authentication token. [y/N]\e[0m ")" -n 1 -r
        echo
        if [[ $REPLY =~ ^[Yy]$ ]]; then
            log_info "Removing user data directory: $HOME/.urnetwork"
            rm -rf "$HOME/.urnetwork"
        else
            log_info "Skipping removal of user data directory."
        fi
    fi

    # Final legacy cleanup
    [ -d "$OLD_INSTALL_PATH" ] && rm -rf "$OLD_INSTALL_PATH"

    log_success "\nUninstallation complete."
    log_info "Please restart your shell for PATH changes to take full effect."
}

do_update() {
    is_quiet=false
    if [[ "$1" == "--quiet" ]]; then
        is_quiet=true
    fi

    if [ "$is_quiet" = true ]; then
        # Throttle automatic checks to once every 24 hours
        if [ -f "$LAST_UPDATE_CHECK_FILE" ]; then
            local last_check_time
            last_check_time=$(stat -c %Y "$LAST_UPDATE_CHECK_FILE")
            local current_time
            current_time=$(date +%s)
            if (( current_time - last_check_time < 86400 )); then
                exit 0 # Exit silently if checked within 24 hours
            fi
        fi
        touch "$LAST_UPDATE_CHECK_FILE"
    fi

    ! $is_quiet && log_info "Checking for updates..."
    check_dep "curl" && check_json_parser
    
    local installed_version="none"
    [ -f "$VERSION_FILE" ] && installed_version=$(cat "$VERSION_FILE")
    
    local latest_url
    latest_url=$(get_download_url "latest")
    local latest_version
    latest_version=$(echo "$latest_url" | grep -oP '(?<=/download/)[^/]+')

    if [ -z "$latest_version" ]; then
        ! $is_quiet && log_error "Could not determine the latest version."
        exit 1
    fi

    if [[ "$installed_version" == "$latest_version" ]]; then
        ! $is_quiet && log_success "You are already on the latest version: $installed_version"
        exit 0
    fi
    
    ! $is_quiet && log_info "New version available: $latest_version (you have $installed_version)"
    ! $is_quiet && log_info "Proceeding with update..."
    
    # Re-run the install function which will handle the upgrade logic
    do_install "$latest_version"
}

show_version() {
    local installed_version="Not Installed"
    [ -f "$VERSION_FILE" ] && installed_version=$(cat "$VERSION_FILE")
    echo "Urnetwork Manager Script Version: $SCRIPT_VERSION"
    echo "Urnetwork Provider Installed Version: $installed_version"
}

show_help() {
    echo "Urnetwork Provider Manager ($SCRIPT_VERSION)"
    echo "Usage: $ME [command]"
    echo ""
    echo "Commands:"
    echo "  (no command)    Installs or upgrades the urnetwork provider."
    echo "  --update        Checks for and installs the latest version of the provider."
    echo "  --uninstall     Removes the provider and all related configuration."
    echo "  --version       Displays the version of the manager and the installed provider."
    echo "  --help          Shows this help message."
}


# --- Main Execution Logic ---

# If being run via curl | sh, the script name ($0) will be 'bash' or 'sh'.
# In this case, we default to the install action.
is_direct_invocation=false
if [[ "$0" == "$MANAGER_SCRIPT_PATH" ]] || [[ "$0" == "./install.sh" ]]; then
    is_direct_invocation=true
fi

if ! $is_direct_invocation; then
    do_install
    exit 0
fi

case "$1" in
    ""|--install)
        do_install
        ;;
    --uninstall)
        do_uninstall
        ;;
    --update)
        shift
        do_update "$@"
        ;;
    --version)
        show_version
        ;;
    -h|--help)
        show_help
        ;;
    *)
        log_error "Unknown command: $1"
        show_help
        exit 1
        ;;
esac
