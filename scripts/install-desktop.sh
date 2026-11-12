#!/bin/sh
set -eu

fail() { printf 'install-desktop.sh: %s\n' "$*" >&2; exit 1; }
script_directory=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
binary_directory=$script_directory/../desktop/bin
while [ "$#" -gt 0 ]; do
    case "$1" in
        --binary-dir) [ "$#" -ge 2 ] || fail '--binary-dir requires a value'; binary_directory=$2; shift 2 ;;
        -h|--help) printf 'Usage: install-desktop.sh [--binary-dir DIRECTORY]\nInstall the optional GUI and restricted system helper. DESTDIR stages files only.\n'; exit 0 ;;
        *) fail "unknown argument: $1" ;;
    esac
done
for name in scrapefs-gui scrapefs-admin; do
    [ -f "$binary_directory/$name" ] && [ -x "$binary_directory/$name" ] || fail "missing executable: $binary_directory/$name"
done
desktop_source=$script_directory/../packaging/desktop/io.github.zendext.scrapefs.desktop
[ -f "$desktop_source" ] || fail 'desktop entry is missing'
destination_root=${DESTDIR:-}
if [ -n "$destination_root" ]; then
    case "$destination_root" in /*) ;; *) fail 'DESTDIR must be an absolute path' ;; esac
else
    [ "$(id -u)" -eq 0 ] || fail 'host installation must run as root'
    [ -f /etc/scrapefs/config.toml ] || fail 'install the ScrapeFS daemon first'
    [ "$(systemctl show scrapefsd.service --property=LoadState --value)" = loaded ] || fail 'install scrapefsd.service first'
    command -v pkexec >/dev/null 2>&1 || fail 'pkexec is required for system authorization'
fi
# Reject symlink destinations before installing a privileged executable.
for directory in /usr /usr/local /usr/local/bin /usr/local/libexec /usr/local/share /usr/local/share/applications; do
    path=$destination_root$directory
    [ ! -L "$path" ] || fail "destination is a symbolic link: $path"
    [ ! -e "$path" ] || [ -d "$path" ] || fail "destination is not a directory: $path"
done
for path in /usr/local/bin/scrapefs-gui /usr/local/libexec/scrapefs-admin /usr/local/share/applications/io.github.zendext.scrapefs.desktop; do
    [ ! -L "$destination_root$path" ] || fail "destination is a symbolic link: $path"
done
install -d -m 0755 "$destination_root/usr/local/bin" "$destination_root/usr/local/libexec" "$destination_root/usr/local/share/applications"
if [ -z "$destination_root" ]; then
    for directory in /usr /usr/local /usr/local/bin /usr/local/libexec /usr/local/share /usr/local/share/applications; do
        [ "$(stat -c %u "$directory")" -eq 0 ] || fail "destination is not root-owned: $directory"
        [ "$(find "$directory" -maxdepth 0 -perm /022 -print)" = '' ] || fail "destination is writable by other users: $directory"
    done
fi
install -m 0755 "$binary_directory/scrapefs-gui" "$destination_root/usr/local/bin/scrapefs-gui"
install -m 0755 "$binary_directory/scrapefs-admin" "$destination_root/usr/local/libexec/scrapefs-admin"
install -m 0644 "$desktop_source" "$destination_root/usr/local/share/applications/io.github.zendext.scrapefs.desktop"
if [ -z "$destination_root" ]; then
    chown root:root /usr/local/bin/scrapefs-gui /usr/local/libexec/scrapefs-admin /usr/local/share/applications/io.github.zendext.scrapefs.desktop
fi
printf 'Desktop components installed. Run scrapefs-gui as your normal user.\n'
