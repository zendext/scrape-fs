#!/bin/sh
set -eu

usage() {
    cat <<'EOF'
Usage: package-release.sh VERSION [OUTPUT_DIRECTORY]

Build and package a versioned ScrapeFS release for linux/amd64.
OUTPUT_DIRECTORY defaults to dist and must be empty when it already exists.
EOF
}

fail() {
    printf 'package-release.sh: %s\n' "$*" >&2
    exit 1
}

require_command() {
    command -v "$1" >/dev/null 2>&1 ||
        fail "required command is unavailable: $1"
}

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
    usage >&2
    exit 1
fi

version=$1
output_directory=${2:-dist}

script_directory=$(
    CDPATH= cd -- "$(dirname -- "$0")" &&
        pwd
)
repository=$(
    CDPATH= cd -- "$script_directory/.." &&
        pwd
)

for path in \
    "$repository/scripts/install.sh" \
    "$repository/scripts/release-version.sh" \
    "$repository/packaging/systemd/scrapefsd.service"
do
    [ -f "$path" ] ||
        fail "required release file is missing: $path"
done

"$repository/scripts/release-version.sh" "$version" >/dev/null ||
    fail "invalid release version: $version"

for command in find go install mkdir mktemp sha256sum tar; do
    require_command "$command"
done

[ "$(go env GOHOSTOS)" = "linux" ] &&
    [ "$(go env GOHOSTARCH)" = "amd64" ] ||
    fail "release packaging requires a linux/amd64 build host"

if [ -L "$output_directory" ]; then
    fail "output directory is a symbolic link: $output_directory"
fi
if [ -e "$output_directory" ]; then
    [ -d "$output_directory" ] ||
        fail "output path is not a directory: $output_directory"
    if [ -n "$(
        find "$output_directory" \
            -mindepth 1 \
            -maxdepth 1 \
            -print \
            -quit
    )" ]; then
        fail "output directory is not empty: $output_directory"
    fi
else
    mkdir -p "$output_directory"
fi
output_directory=$(
    CDPATH= cd -- "$output_directory" &&
        pwd
)

archive_root="scrape-fs_${version}_linux_amd64"
archive_name="${archive_root}.tar.gz"
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT HUP INT TERM
package_directory=$temporary/$archive_root
install -d -m 0755 "$package_directory"

ldflags="-s -w -X github.com/zendext/scrape-fs/internal/buildinfo.Version=$version"
(
    cd "$repository"
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
        go build \
        -trimpath \
        -ldflags "$ldflags" \
        -o "$package_directory/scrapefs" \
        ./cmd/scrapefs
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
        go build \
        -trimpath \
        -ldflags "$ldflags" \
        -o "$package_directory/scrapefsd" \
        ./cmd/scrapefsd
)

install -m 0755 \
    "$repository/scripts/install.sh" \
    "$package_directory/install.sh"
install -m 0644 \
    "$repository/packaging/systemd/scrapefsd.service" \
    "$package_directory/scrapefsd.service"

for name in scrapefs scrapefsd; do
    actual=$("$package_directory/$name" --version)
    [ "$actual" = "$name $version" ] ||
        fail "$name version output is $actual, expected $name $version"
done

tar -C "$temporary" -czf "$output_directory/$archive_name" "$archive_root"
(
    cd "$output_directory"
    sha256sum "$archive_name" >SHA256SUMS
)

printf 'Created %s\n' "$output_directory/$archive_name"
printf 'Created %s\n' "$output_directory/SHA256SUMS"
