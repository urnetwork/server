#!/usr/bin/env bash

# Deterministic wall-clock budgets shared by evaluator validation and tests.

set -Eeuo pipefail

readonly EVALUATION_STAGE_OVERHEAD_SECONDS=600
readonly EVALUATION_JOB_OVERHEAD_SECONDS=1200

die() { printf '[competition-timeout-budget] ERROR: %s\n' "$*" >&2; exit 2; }
require_nonnegative_integer() {
    local name="$1" value="$2"
    [[ "$value" =~ ^[0-9]+$ ]] || die "$name must be a nonnegative integer"
}
require_positive_integer() {
    local name="$1" value="$2"
    [[ "$value" =~ ^[1-9][0-9]*$ ]] || die "$name must be a positive integer"
}

stage_timeout_seconds() {
    [ "$#" -eq 5 ] || die "stage requires five millisecond values"
    local ramp_ms="$1" settle_ms="$2" client_warmup_timeout_ms="$3"
    local duration_ms="$4" request_timeout_ms="$5" value
    require_nonnegative_integer ramp_ms "$ramp_ms"
    require_nonnegative_integer settle_ms "$settle_ms"
    require_positive_integer client_warmup_timeout_ms "$client_warmup_timeout_ms"
    require_positive_integer duration_ms "$duration_ms"
    require_positive_integer request_timeout_ms "$request_timeout_ms"
    value=$(((ramp_ms + settle_ms + client_warmup_timeout_ms + duration_ms + request_timeout_ms + 999) / 1000 + EVALUATION_STAGE_OVERHEAD_SECONDS))
    printf '%s\n' "$value"
}

minimum_score_timeout_seconds() {
    [ "$#" -eq 6 ] || die "score requires a replicate count and five millisecond values"
    local replicates="$1" stage_seconds
    shift
    require_positive_integer replicates "$replicates"
    stage_seconds="$(stage_timeout_seconds "$@")"
    printf '%s\n' "$((2 * replicates * stage_seconds + EVALUATION_JOB_OVERHEAD_SECONDS))"
}

case "${1:-}" in
    stage)
        shift
        stage_timeout_seconds "$@"
        ;;
    score)
        shift
        minimum_score_timeout_seconds "$@"
        ;;
    *)
        die "usage: timeout-budget.sh {stage|score} ..."
        ;;
esac
