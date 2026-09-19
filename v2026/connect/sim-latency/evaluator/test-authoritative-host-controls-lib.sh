#!/usr/bin/env bash

set -Eeuo pipefail
umask 077

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
readonly LIBRARY="$SCRIPT_DIR/authoritative-host-controls-lib.sh"

test_root="$(mktemp -d)"
cleanup() {
    rm -rf -- "$test_root"
}
trap cleanup EXIT INT TERM

# shellcheck source=authoritative-host-controls-lib.sh
source "$LIBRARY"

control="$test_root/control"
trace="$test_root/trace"

# Reproduce the boot defect: the first `off` transition fails while the
# control reports numeric `1`; `on` normalizes it and the next `off` succeeds.
printf '1\n' >"$control"
urnetwork_smt_write() {
    local path="$1" value="$2" state
    state="$(tr -d '\n' <"$path")"
    printf '%s:%s\n' "$state" "$value" >>"$trace"
    case "$state:$value" in
        1:off)
            return 1
            ;;
        1:on)
            printf 'on\n' >"$path"
            ;;
        on:off)
            printf 'off\n' >"$path"
            ;;
        *)
            return 1
            ;;
    esac
}
urnetwork_disable_smt "$control" 1 0
[ "$(tr -d '\n' <"$control")" = off ]
[ "$(tr '\n' ',' <"$trace")" = '1:on,on:off,' ]

# An already-disabled host is a no-op.
: >"$trace"
urnetwork_disable_smt "$control" 1 0
[ ! -s "$trace" ]

# A persistent write failure remains fail-closed after the requested attempts.
printf 'on\n' >"$control"
attempt_count=0
urnetwork_smt_write() {
    attempt_count=$((attempt_count + 1))
    return 1
}
if urnetwork_disable_smt "$control" 3 0; then
    printf 'persistent SMT write failure passed\n' >&2
    exit 1
fi
[ "$attempt_count" -eq 3 ]

printf 'authoritative SMT normalization tests passed\n'
