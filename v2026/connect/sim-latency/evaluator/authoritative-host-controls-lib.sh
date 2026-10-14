#!/usr/bin/env bash

# Shared, side-effect-free helpers for the authoritative host-control command.
# This file is sourced by the root-owned installer target and by its regression
# test; it is not an executable entry point.

urnetwork_smt_state() {
    local control_path="$1"
    tr -d '\n' <"$control_path" 2>/dev/null || true
}

urnetwork_smt_write() {
    local control_path="$1" value="$2"
    if [ -w "$control_path" ]; then
        printf '%s\n' "$value" >"$control_path"
    else
        printf '%s\n' "$value" | sudo -n tee "$control_path" >/dev/null
    fi
}

urnetwork_disable_smt() {
    local control_path="$1" attempts="${2:-10}" retry_delay="${3:-1}"
    local attempt state

    [[ "$attempts" =~ ^[1-9][0-9]*$ ]] || return 2
    [ -e "$control_path" ] || return 1
    for ((attempt = 1; attempt <= attempts; attempt++)); do
        state="$(urnetwork_smt_state "$control_path")"
        case "$state" in
            off|forceoff|notsupported)
                return 0
                ;;
            on)
                ;;
            *)
                # Some kernels expose a transient numeric state while CPU
                # hotplug is still settling. Normalizing it to `on` makes the
                # subsequent `off` transition deterministic.
                urnetwork_smt_write "$control_path" on >/dev/null 2>&1 || true
                ;;
        esac
        if urnetwork_smt_write "$control_path" off >/dev/null 2>&1; then
            state="$(urnetwork_smt_state "$control_path")"
            case "$state" in
                off|forceoff|notsupported)
                    return 0
                    ;;
            esac
        fi
        if [ "$attempt" -lt "$attempts" ] && [ "$retry_delay" != 0 ]; then
            sleep "$retry_delay"
        fi
    done
    return 1
}
