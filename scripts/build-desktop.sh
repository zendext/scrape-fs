#!/bin/sh
set -eu

script_directory=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repository=$(CDPATH= cd -- "$script_directory/.." && pwd)
[ "$(go env GOHOSTOS)" = linux ] || { printf 'Desktop builds require Linux.\n' >&2; exit 1; }

cd "$repository/desktop/frontend"
npm ci
npm run build
cd "$repository/desktop"
mkdir -p bin
go build -trimpath -tags production -o bin/scrapefs-gui .
cd "$repository"
go build -trimpath -o desktop/bin/scrapefs-admin ./cmd/scrapefs-admin
printf 'Built desktop/bin/scrapefs-gui and desktop/bin/scrapefs-admin\n'
