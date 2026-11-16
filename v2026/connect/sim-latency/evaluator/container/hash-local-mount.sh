#!/usr/bin/env bash

# Emit the deterministic manifest digest frozen into config/competition.yml
# for one direct config/local or vault/local bind mount.

set -Eeuo pipefail
export LANG=C LC_ALL=C

if [ "$#" -ne 1 ]; then
    printf 'usage: hash-local-mount.sh ABSOLUTE_LOCAL_DIRECTORY\n' >&2
    exit 2
fi

root="$1"
canonical_root="$(
    builtin cd -P -- "$root" 2>/dev/null && builtin pwd -P
)" || canonical_root=""
[[ "$root" = /* ]] && [ "$root" = "$canonical_root" ] &&
    [ -d "$root" ] && [ ! -L "$root" ] || {
        printf 'local mount root must be an absolute canonical directory\n' >&2
        exit 1
    }
[ -z "$(find "$root" -mindepth 1 ! -type d ! -type f -print -quit)" ] || {
    printf 'local mount contains a non-regular entry\n' >&2
    exit 1
}

if command -v sha256sum >/dev/null 2>&1; then
    hash_file() {
        sha256sum < "$1" | awk '{print $1}'
    }
    hash_stream() {
        sha256sum | awk '{print $1}'
    }
elif command -v shasum >/dev/null 2>&1; then
    hash_file() {
        shasum -a 256 < "$1" | awk '{print $1}'
    }
    hash_stream() {
        shasum -a 256 | awk '{print $1}'
    }
else
    printf 'SHA-256 utility is unavailable\n' >&2
    exit 1
fi

(
    cd -- "$root"
    find . -type f -print0 | sort -z | while IFS= read -r -d '' relative; do
        relative="${relative#./}"
        case "$relative" in
            *$'\n'*|*$'\r'*)
                printf 'local mount contains an unsafe path\n' >&2
                exit 1
                ;;
        esac
        digest="$(hash_file "$root/$relative")" || exit 1
        printf '%s  %s\n' "$digest" "$relative"
    done
) | hash_stream
