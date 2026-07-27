#!/bin/sh
set -eu

if [ "$#" -ne 1 ]; then
    printf 'release-version.sh: expected exactly one version\n' >&2
    exit 1
fi

version=$1
case "$version" in
    v[0-9]*)
        case "$version" in
            *[!0-9A-Za-z.+-]*)
                printf 'release-version.sh: invalid release version: %s\n' \
                    "$version" >&2
                exit 1
                ;;
        esac
        ;;
    *)
        printf 'release-version.sh: invalid release version: %s\n' \
            "$version" >&2
        exit 1
        ;;
esac

case "$version" in
    *-*)
        printf 'prerelease\n'
        ;;
    *)
        printf 'stable\n'
        ;;
esac
