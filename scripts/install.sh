#!/bin/sh
set -eu

usage() {
    cat <<'EOF'
Usage: install.sh --administrator USER [--binary-dir DIRECTORY]

Install ScrapeFS binaries, host directories, default configuration, and the
generic systemd service unit.

Options:
  --administrator USER    Existing user authorized to administer ScrapeFS.
  --binary-dir DIRECTORY  Directory containing scrapefs and scrapefsd.
                          Defaults to the directory containing this script.
  -h, --help              Show this help.

Set DESTDIR to stage the installation without requiring root or modifying
users, groups, or systemd state.
EOF
}

fail() {
    printf 'install.sh: %s\n' "$*" >&2
    exit 1
}

require_command() {
    command -v "$1" >/dev/null 2>&1 ||
        fail "required command is unavailable: $1"
}

script_directory=$(
    CDPATH= cd -- "$(dirname -- "$0")" &&
        pwd
)
administrator=
binary_directory=$script_directory

while [ "$#" -gt 0 ]; do
    case "$1" in
        --administrator)
            [ "$#" -ge 2 ] ||
                fail "--administrator requires a value"
            administrator=$2
            shift 2
            ;;
        --binary-dir)
            [ "$#" -ge 2 ] ||
                fail "--binary-dir requires a value"
            binary_directory=$2
            shift 2
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            fail "unknown argument: $1"
            ;;
    esac
done

[ -n "$administrator" ] ||
    fail "--administrator is required"
[ -d "$binary_directory" ] ||
    fail "binary directory does not exist: $binary_directory"

binary_directory=$(
    CDPATH= cd -- "$binary_directory" &&
        pwd
)
for name in scrapefs scrapefsd; do
    [ -f "$binary_directory/$name" ] &&
        [ -x "$binary_directory/$name" ] ||
        fail "missing executable: $binary_directory/$name"
done

if [ -f "$script_directory/scrapefsd.service" ]; then
    service_source=$script_directory/scrapefsd.service
elif [ -f "$script_directory/../packaging/systemd/scrapefsd.service" ]; then
    service_source=$script_directory/../packaging/systemd/scrapefsd.service
else
    fail "cannot find the generic scrapefsd.service unit"
fi

administrator_uid=$(id -u "$administrator" 2>/dev/null) ||
    fail "administrator user does not exist: $administrator"

require_command install
require_command mktemp
require_command ln
require_command mkdir
require_command findmnt

destination_root=${DESTDIR:-}
install_path() {
    printf '%s%s\n' "$destination_root" "$1"
}

binary_install_directory=$(install_path /usr/local/bin)
configuration_directory=$(install_path /etc/scrapefs)
configuration_path=$configuration_directory/config.toml
state_directory=$(install_path /var/lib/scrapefs)
mountpoint=$(install_path /mnt/scrapefs)
unit_directory=$(install_path /etc/systemd/system)
unit_path=$unit_directory/scrapefsd.service

validate_managed_directory() {
    managed_directory=$1
    if [ -L "$managed_directory" ]; then
        fail "managed directory is a symbolic link: $managed_directory"
    fi
    if [ -e "$managed_directory" ] &&
        [ ! -d "$managed_directory" ]
    then
        fail "managed path is not a directory: $managed_directory"
    fi
}

for managed_directory in \
    "$configuration_directory" \
    "$state_directory" \
    "$mountpoint"
do
    validate_managed_directory "$managed_directory"
done

if findmnt -rn -M "$mountpoint" >/dev/null 2>&1; then
    fail "mountpoint is currently mounted: $mountpoint; stop scrapefsd.service before installing"
fi

if [ -z "$destination_root" ]; then
    [ "$(id -u)" -eq 0 ] ||
        fail "host installation must run as root"
    for command in getent groupadd usermod systemctl; do
        require_command "$command"
    done

    getent group scrapefs >/dev/null 2>&1 ||
        groupadd --system scrapefs
    getent group scrapefs-control >/dev/null 2>&1 ||
        groupadd --system scrapefs-control
    if ! id -nG "$administrator" |
        tr ' ' '\n' |
        grep -Fxq scrapefs-control
    then
        usermod -aG scrapefs-control "$administrator"
    fi
fi

if [ -n "$destination_root" ]; then
    mkdir -p "$binary_install_directory" "$unit_directory"
    install -d -m 0755 "$configuration_directory"
    install -d -m 0700 "$state_directory"
    install -d -m 0710 "$mountpoint"
    install -m 0755 \
        "$binary_directory/scrapefs" \
        "$binary_directory/scrapefsd" \
        "$binary_install_directory/"
    install -m 0644 "$service_source" "$unit_path"
else
    mkdir -p "$binary_install_directory" "$unit_directory"
    install -d -o root -g root -m 0755 "$configuration_directory"
    install -d -o root -g root -m 0700 "$state_directory"
    install -d -o root -g scrapefs -m 0710 "$mountpoint"
    install -o root -g root -m 0755 \
        "$binary_directory/scrapefs" \
        "$binary_directory/scrapefsd" \
        "$binary_install_directory/"
    install -o root -g root -m 0644 "$service_source" "$unit_path"
fi

if [ ! -e "$configuration_path" ]; then
    temporary=$(mktemp "$configuration_directory/.config.toml.XXXXXX")
    trap 'rm -f "$temporary"' EXIT HUP INT TERM
    cat >"$temporary" <<EOF
[storage]
database_path = "/var/lib/scrapefs/scrapefs.db"
overlay_store_path = "/var/lib/scrapefs/objects"

[fuse]
mountpoint = "/mnt/scrapefs"
entry_timeout = "2s"
attr_timeout = "2s"
negative_timeout = "0s"

[control]
socket_path = "/run/scrapefs/scrapefs.sock"
socket_mode = 0o660
administrator_uid = $administrator_uid

[gc]
orphan_grace_period = "24h"
EOF
    chmod 0644 "$temporary"
    if [ -z "$destination_root" ]; then
        chown root:root "$temporary"
    fi
    if ln "$temporary" "$configuration_path" 2>/dev/null; then
        printf 'Created %s\n' "$configuration_path"
    else
        printf 'Preserved existing %s\n' "$configuration_path"
    fi
    rm -f "$temporary"
    trap - EXIT HUP INT TERM
else
    printf 'Preserved existing %s\n' "$configuration_path"
fi

if [ -z "$destination_root" ]; then
    systemctl daemon-reload
    printf '%s\n' \
        "Installed ScrapeFS for administrator $administrator." \
        "Review $configuration_path, then run:" \
        "  systemctl enable --now scrapefsd.service"
else
    printf 'Staged ScrapeFS under %s\n' "$destination_root"
fi
