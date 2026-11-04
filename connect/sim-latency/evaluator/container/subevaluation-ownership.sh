#!/usr/bin/env bash

# Source only from the protected prospective evaluator release. The agent pins
# these exact bytes before launch; no candidate file selects labels or cgroups.
# No environment means legacy behavior. Any partial scope refuses all work.

subevaluation_ownership_init() {
    subevaluation_scoped=false
    subevaluation_run_id=""
    subevaluation_preflight_cgroup=""
    subevaluation_docker_args=()
    subevaluation_run_label_args=()
    subevaluation_compose_args=()
    subevaluation_cleanup_filter_args=()
    local present=0 name value directory token
    for name in URNETWORK_SUBEVALUATION_RUN_ID URNETWORK_SUBEVALUATION_JOB_ID URNETWORK_SUBEVALUATION_ROUND_ID URNETWORK_SUBEVALUATION_ATTEMPT; do
        if [[ -v "$name" ]]; then
            present=$((present + 1))
        fi
    done
    [[ "$present" != 0 ]] || return 0
    if [[ "$present" != 4 || ! "${URNETWORK_SUBEVALUATION_RUN_ID:-}" =~ ^[0-9a-f]{64}$ ||
          ! "${URNETWORK_SUBEVALUATION_JOB_ID:-}" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ||
          ! "${URNETWORK_SUBEVALUATION_ROUND_ID:-}" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ||
          ! "${URNETWORK_SUBEVALUATION_ATTEMPT:-}" =~ ^[1-9][0-9]{0,18}$ ]]; then
        printf 'subevaluation ownership scope is incomplete or malformed\n' >&2
        return 2
    fi
    value="$URNETWORK_SUBEVALUATION_ATTEMPT"
    if [[ "${#value}" = 19 && "$value" > 9223372036854775807 ]]; then
        printf 'subevaluation ownership attempt exceeds its signed integer bound\n' >&2
        return 2
    fi
    token="$(printf '%s:%s' "$URNETWORK_SUBEVALUATION_JOB_ID" "$value" | sha256sum)" || return 2
    token="${token%% *}"
    [[ "$token" =~ ^[0-9a-f]{64}$ ]] || return 2
    directory="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)" || return 2
    subevaluation_run_id="$URNETWORK_SUBEVALUATION_RUN_ID"
    subevaluation_preflight_cgroup="urnetwork-evaluation-${token:0:32}-preflight.slice"
    subevaluation_docker_args=(
        --label "com.urnetwork.competition.job-id=$URNETWORK_SUBEVALUATION_JOB_ID"
        --label "com.urnetwork.competition.round-id=$URNETWORK_SUBEVALUATION_ROUND_ID"
        --label "com.urnetwork.competition.run-id=$subevaluation_run_id"
        --cgroup-parent "$subevaluation_preflight_cgroup"
    )
    subevaluation_run_label_args=(--label "com.urnetwork.competition.run-id=$subevaluation_run_id")
    subevaluation_compose_args=(--file "$directory/compose.subevaluation.yml")
    subevaluation_cleanup_filter_args=(
        --filter "label=com.urnetwork.competition.round-id=$URNETWORK_SUBEVALUATION_ROUND_ID"
        --filter "label=com.urnetwork.competition.run-id=$subevaluation_run_id"
    )
    subevaluation_scoped=true
}

# Bind the evaluator's decoded immutable request before any resource creation.
subevaluation_require_identity() {
    if [[ "${subevaluation_scoped:-false}" = true ]]; then
        [[ "$#" = 3 && "$1" = "$URNETWORK_SUBEVALUATION_JOB_ID" &&
           "$2" = "$URNETWORK_SUBEVALUATION_ROUND_ID" && "$3" = "$URNETWORK_SUBEVALUATION_ATTEMPT" ]] || {
            printf 'subevaluation request differs from its authenticated ownership scope\n' >&2
            return 2
        }
    fi
}
