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
    subevaluation_docker_command=(sudo -n docker)
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
    if [[ -v URNETWORK_SUBEVALUATION_CORE_PROFILE && "$URNETWORK_SUBEVALUATION_CORE_PROFILE" != "$directory/candidate-core.seccomp.json" ]]; then
        printf 'subevaluation core profile differs from its protected helper release\n' >&2
        return 2
    fi
    export URNETWORK_SUBEVALUATION_CORE_PROFILE="$directory/candidate-core.seccomp.json"
    subevaluation_run_id="$URNETWORK_SUBEVALUATION_RUN_ID"
    subevaluation_docker_command=("${URNETWORK_SUBEVALUATION_CLIENT:-}" --docker-client "$subevaluation_run_id" --)
    subevaluation_preflight_cgroup="urnetwork-evaluation-${token:0:32}-preflight.slice"
    export URNETWORK_SUBEVALUATION_BRIDGE_NAME="unev-${subevaluation_run_id:0:10}"
    subevaluation_management_args=(--cpuset-cpus "${URNETWORK_SUBEVALUATION_MANAGEMENT_CPUSET:-}")
    subevaluation_docker_args=(
        --label "com.urnetwork.competition.job-id=$URNETWORK_SUBEVALUATION_JOB_ID"
        --label "com.urnetwork.competition.round-id=$URNETWORK_SUBEVALUATION_ROUND_ID"
        --label "com.urnetwork.competition.run-id=$subevaluation_run_id"
        --cgroup-parent "$subevaluation_preflight_cgroup"
        --ulimit core=1:1
        --security-opt "seccomp=$URNETWORK_SUBEVALUATION_CORE_PROFILE"
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

# Measurement cannot overlap retained unbounded BuildKit memory. Only the
# no-argument root role retires it; this helper retains its finite receipt.
# The unscoped evaluator never calls this prospective phase boundary.
subevaluation_retire_build() {
    [[ "${subevaluation_scoped:-false}" = true && "$#" = 1 && "$1" = /* &&
       "$1" != *$'\n'* && "$1" != *$'\r'* ]] || return 2
    local destination="$1" pending="$1.new"
    [[ ! -e "$destination" && ! -L "$destination" && ! -e "$pending" && ! -L "$pending" ]] || return 2
    if ! (set -o noclobber; evaluation_host retire-build > "$pending"); then
        rm -f -- "$pending"
        return 2
    fi
    if ! jq -e '
        type == "object" and
        (keys | sort) == ["memory_current_bytes","memory_stat_sha256","outcome_kind","receipt_sha256","retained_memory_maximum_bytes"] and
        (.outcome_kind == "acknowledged-build" or .outcome_kind == "no-build-issued") and
        (.retained_memory_maximum_bytes | type == "number" and . > 0 and floor == .) and
        (.memory_current_bytes | type == "number" and . >= 0 and floor == .) and
        .memory_current_bytes <= .retained_memory_maximum_bytes and
        (.memory_stat_sha256 | type == "string" and test("^[0-9a-f]{64}$")) and
        (.receipt_sha256 | type == "string" and test("^[0-9a-f]{64}$"))
    ' "$pending" >/dev/null; then
        rm -f -- "$pending"
        return 2
    fi
    chmod 0400 "$pending" || return
    sync -d "$pending" || return
    mv -- "$pending" "$destination" || return
    sync "$(dirname "$destination")"
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

# Product/source data must not gain a host process through Git hooks, fsmonitor,
# global configuration or protocol helpers after a checked patch is applied.
# Broad writes additionally use the independently pinned canonical validator.
evaluation_git() {
    if [[ "${subevaluation_scoped:-false}" = true ]]; then
        local argument repository="" previous="" configured status=0
        local -a safe_arguments=()
        local command_seen=false option_value=false
        for argument in "$@"; do
            if [[ "$previous" = -C ]]; then repository="$argument"; fi
            previous="$argument"
            safe_arguments+=("$argument")
            if [[ "$option_value" = true ]]; then option_value=false; continue; fi
            if [[ "$command_seen" != true ]]; then
                case "$argument" in
                    -C|-c|--git-dir|--work-tree) option_value=true ;;
                    -*) ;;
                    *)
                        command_seen=true
                        if [[ "$argument" = diff ]]; then safe_arguments+=(--no-ext-diff --no-textconv); fi
                        ;;
                esac
            fi
        done
        [[ "$repository" = /* && -d "$repository/.git" && ! -L "$repository/.git" ]] || return 2
        configured="$(GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_SYSTEM=/dev/null GIT_CONFIG_GLOBAL=/dev/null \
            git --no-pager -c core.hooksPath=/dev/null -c core.fsmonitor=false -C "$repository" \
                config --local --includes --name-only --get-regexp '^filter\.')" || status=$?
        [[ "$status" = 1 && -z "$configured" ]] || {
            printf 'source repository contains an unqualified Git content filter\n' >&2
            return 2
        }
        GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_SYSTEM=/dev/null GIT_CONFIG_GLOBAL=/dev/null \
        GIT_ATTR_NOSYSTEM=1 GIT_TERMINAL_PROMPT=0 \
            git --no-pager -c core.hooksPath=/dev/null -c core.fsmonitor=false \
                -c protocol.allow=never -c gc.auto=0 -c maintenance.auto=false "${safe_arguments[@]}"
    else
        git "$@"
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
