#!/bin/sh
set -eu

script_directory=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repository=$(CDPATH= cd -- "$script_directory/.." && pwd)
[ "$(go env GOHOSTOS)" = linux ] || { printf 'Desktop builds require Linux.\n' >&2; exit 1; }
[ "$#" -le 1 ] || { printf 'Usage: build-desktop.sh [RELEASE_VERSION]\n' >&2; exit 1; }
ldflags=
if [ "$#" -eq 1 ]; then
    "$script_directory/release-version.sh" "$1" >/dev/null
    ldflags="-s -w -X github.com/zendext/scrape-fs/internal/buildinfo.Version=$1"
fi

cd "$repository/desktop/frontend"
npm ci
npm run build
cd "$repository/desktop"
mkdir -p bin
CGO_ENABLED=1 go build -trimpath -tags production -ldflags "$ldflags" -o bin/scrapefs-gui .
cd "$repository"
CGO_ENABLED=0 go build -trimpath -ldflags "$ldflags" -o desktop/bin/scrapefs-admin ./cmd/scrapefs-admin
printf 'Built desktop/bin/scrapefs-gui and desktop/bin/scrapefs-admin\n'
