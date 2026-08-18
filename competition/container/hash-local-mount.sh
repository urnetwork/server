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
[[ "$root" = /* ]] && [ "$root" = "$(realpath -e "$root")" ] &&
    [ -d "$root" ] && [ ! -L "$root" ] || {
        printf 'local mount root must be an absolute canonical directory\n' >&2
        exit 1
    }
[ -z "$(find "$root" -mindepth 1 ! -type d ! -type f -print -quit)" ] || {
    printf 'local mount contains a non-regular entry\n' >&2
    exit 1
}

(
    while IFS= read -r -d '' relative; do
        case "$relative" in
            *$'\n'*|*$'\r'*)
                printf 'local mount contains an unsafe path\n' >&2
                exit 1
                ;;
        esac
        printf '%s  %s\n' "$(sha256sum "$root/$relative" | awk '{print $1}')" "$relative"
    done < <(find "$root" -type f -printf '%P\0' | sort -z)
) | sha256sum | awk '{print $1}'
