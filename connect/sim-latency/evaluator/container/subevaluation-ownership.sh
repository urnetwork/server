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
    subevaluation_probe_args=()
    subevaluation_management_args=()
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
    export URNETWORK_SUBEVALUATION_BRIDGE_NAME="unev-${subevaluation_run_id:0:10}"
    subevaluation_management_args=(--cpuset-cpus "${URNETWORK_SUBEVALUATION_MANAGEMENT_CPUSET:-}")
    subevaluation_docker_args=(
        --label "com.urnetwork.competition.job-id=$URNETWORK_SUBEVALUATION_JOB_ID"
        --label "com.urnetwork.competition.round-id=$URNETWORK_SUBEVALUATION_ROUND_ID"
        --label "com.urnetwork.competition.run-id=$subevaluation_run_id"
        --cgroup-parent "$subevaluation_preflight_cgroup"
        --ulimit core=0:0
    )
    subevaluation_probe_args=(
        --network none --read-only --cap-drop ALL
        --security-opt no-new-privileges:true
        --memory 4g --memory-swap 4g --pids-limit 1024
        "${subevaluation_management_args[@]}"
    )
    subevaluation_run_label_args=(--label "com.urnetwork.competition.run-id=$subevaluation_run_id")
    subevaluation_compose_args=(--file "$directory/compose.subevaluation.yml")
    subevaluation_cleanup_filter_args=(
        --filter "label=com.urnetwork.competition.round-id=$URNETWORK_SUBEVALUATION_ROUND_ID"
        --filter "label=com.urnetwork.competition.run-id=$subevaluation_run_id"
    )
    subevaluation_scoped=true
}

# Only the root-pinned public role can route prospective Docker calls. Neither
# an inherited Docker context nor a generic sudo capability is used in scope.
subevaluation_require_client() {
    [[ "${URNETWORK_SUBEVALUATION_CLIENT:-}" == /* &&
       "${URNETWORK_SUBEVALUATION_CLIENT:-}" != *$'\n'* &&
       "${URNETWORK_SUBEVALUATION_CLIENT:-}" != *$'\r'* &&
       -f "$URNETWORK_SUBEVALUATION_CLIENT" && -x "$URNETWORK_SUBEVALUATION_CLIENT" &&
       ! -L "$URNETWORK_SUBEVALUATION_CLIENT" &&
       "${URNETWORK_SUBEVALUATION_MANAGEMENT_CPUSET:-}" =~ ^[0-9]+([,-][0-9]+)*$ ]] || {
        printf 'subevaluation has no fixed non-root client and management CPU identity\n' >&2
        return 2
    }
}

# The unscoped branch retains the old command and argument order. Scope is
# supplied only by the authenticated agent, never by a candidate container.
evaluation_docker() {
    if [[ "${subevaluation_scoped:-false}" = true ]]; then
        subevaluation_require_client || return
        "$URNETWORK_SUBEVALUATION_CLIENT" --docker-client "$subevaluation_run_id" -- "$@"
    else
        sudo -n docker "$@"
    fi
}

# Ordinary helper cleanup gets the same external bound without giving the
# worker root permission to run an arbitrary timeout/command combination.
evaluation_timed_docker() {
    if [[ "${subevaluation_scoped:-false}" = true ]]; then
        subevaluation_require_client || return
        timeout --signal=TERM --kill-after=1s 3s "$URNETWORK_SUBEVALUATION_CLIENT" --docker-client "$subevaluation_run_id" -- "$@"
    else
        sudo -n timeout --signal=TERM --kill-after=1s 3s docker "$@"
    fi
}

# Root filesystem work has a finite positional vocabulary in the independent
# owner. No shell, executable, uid/gid, mount source or mode crosses this call.
evaluation_host() {
    [[ "${subevaluation_scoped:-false}" = true ]] || return 2
    subevaluation_require_client || return
    "$URNETWORK_SUBEVALUATION_CLIENT" --host-operation "$subevaluation_run_id" -- "$@"
}

# Container output is inspected only after its complete cohort has stopped and
# the descriptor-confined broker has reclaimed it. Scoped work stays non-root.
evaluation_file() {
    if [[ "${subevaluation_scoped:-false}" = true ]]; then
        "$@"
    else
        sudo -n "$@"
    fi
}

# Read-only input binds use worker-owned, world-readable leaves behind the
# private run ancestor. Only legacy mode needs translated ownership on copies.
evaluation_sealed_copy() {
    [[ "$#" = 4 ]] || return 2
    if [[ "${subevaluation_scoped:-false}" = true ]]; then
        install -m 0444 -- "$3" "$4"
    else
        sudo -n install -o "$1" -g "$2" -m 0400 -- "$3" "$4"
    fi
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
