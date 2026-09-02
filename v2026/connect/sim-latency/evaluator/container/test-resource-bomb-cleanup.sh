#!/usr/bin/env bash

# Exercise the production CPU/memory split with simultaneous hostile
# containers, then prove that management-reserved CPUs can clean every object.

set -Eeuo pipefail
umask 077

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
readonly SERVER_ROOT="$(cd "$SCRIPT_DIR/../../../.." && pwd -P)"
readonly RESOURCE_BOUNDARY="$SCRIPT_DIR/resource-boundary.sh"
readonly FIXTURE_ROOT="$SCRIPT_DIR/testdata/resource-bomb"
readonly JOB_ID=bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb
readonly CPU_CONTAINER=urnetwork-resource-bomb-cpu
readonly MEMORY_CONTAINER=urnetwork-resource-bomb-memory
readonly NETWORK=urnetwork-resource-bomb-network
readonly IMAGE=urnetwork/resource-bomb-gate:local
readonly QUICK_MEMORY_BOMB_LIMIT=134217728
readonly CLEANUP_LIMIT_MS=10000

die() { printf '[resource-bomb-gate] ERROR: %s\n' "$*" >&2; exit 1; }

production_memory_limit=false
original_args=("$@")
case "${1:-}" in
    "") ;;
    --production-memory-limit)
        production_memory_limit=true
        shift
        ;;
    *)
        die "usage: test-resource-bomb-cleanup.sh [--production-memory-limit]"
        ;;
esac
[ "$#" -eq 0 ] || die "usage: test-resource-bomb-cleanup.sh [--production-memory-limit]"

for command in awk date go jq mktemp paste realpath seq sleep sort sudo taskset timeout; do
    command -v "$command" >/dev/null 2>&1 || die "required command missing: $command"
done
[ -x "$RESOURCE_BOUNDARY" ] || die "resource boundary command is not executable"

boundary_json="$($RESOURCE_BOUNDARY)"
evaluation_cpuset="$(jq -er '.evaluation_cpuset' <<<"$boundary_json")"
management_cpuset="$(jq -er '.management_cpuset' <<<"$boundary_json")"
memory_bomb_limit="$QUICK_MEMORY_BOMB_LIMIT"
memory_wait_seconds=30
if [ "$production_memory_limit" = true ]; then
    memory_bomb_limit="$(jq -er '.runner_memory_limit_bytes' <<<"$boundary_json")"
    memory_wait_seconds=300
fi
[[ "$memory_bomb_limit" =~ ^[1-9][0-9]*$ ]] || die "memory bomb limit is invalid"

expected_affinity="$(taskset -c "$management_cpuset" awk -F: \
    '$1 == "Cpus_allowed_list" {gsub(/[[:space:]]/, "", $2); print $2}' /proc/self/status)"
if [ "${COMPETITION_MANAGEMENT_CPUSET_APPLIED:-}" != "$management_cpuset" ]; then
    exec env COMPETITION_MANAGEMENT_CPUSET_APPLIED="$management_cpuset" \
        taskset -c "$management_cpuset" "$0" "${original_args[@]}"
fi
actual_affinity="$(awk -F: '$1 == "Cpus_allowed_list" {gsub(/[[:space:]]/, "", $2); print $2}' /proc/self/status)"
[ "$actual_affinity" = "$expected_affinity" ] || die "gate is not confined to management CPUs"

build_context="$(mktemp -d "${TMPDIR:-/tmp}/urnetwork-resource-bomb.XXXXXXXX")"
cleanup_job_objects() {
    while IFS= read -r container_id; do
        [ -n "$container_id" ] && taskset -c "$management_cpuset" sudo -n docker rm -f "$container_id" >/dev/null 2>&1 || true
    done < <(sudo -n docker ps -aq --filter "label=com.urnetwork.competition.job-id=$JOB_ID" 2>/dev/null || true)
    while IFS= read -r network_id; do
        [ -n "$network_id" ] && taskset -c "$management_cpuset" sudo -n docker network rm "$network_id" >/dev/null 2>&1 || true
    done < <(sudo -n docker network ls -q --filter "label=com.urnetwork.competition.job-id=$JOB_ID" 2>/dev/null || true)
}
cleanup() {
    cleanup_job_objects
    chmod -R u+w "$build_context" 2>/dev/null || true
    rm -rf -- "$build_context"
}
trap cleanup EXIT INT TERM

cleanup_job_objects
CGO_ENABLED=0 go build -trimpath -o "$build_context/resource-bomb" \
    "$SERVER_ROOT/connect/sim-latency/evaluator/container/testdata/resource-bomb"
install -m 0444 "$FIXTURE_ROOT/Dockerfile" "$build_context/Dockerfile"
taskset -c "$management_cpuset" sudo -n docker build \
    --network none --provenance=false --file "$build_context/Dockerfile" \
    --tag "$IMAGE" "$build_context" >/dev/null

taskset -c "$management_cpuset" sudo -n docker network create \
    --internal --label "com.urnetwork.competition.job-id=$JOB_ID" "$NETWORK" >/dev/null
common_run=(
    --detach
    --label "com.urnetwork.competition.job-id=$JOB_ID"
    --cgroup-parent "urnetwork-evaluation-${JOB_ID//-/}-bomb.slice"
    --cpuset-cpus "$evaluation_cpuset"
    --memory "$memory_bomb_limit"
    --memory-swap "$memory_bomb_limit"
    --pids-limit 256
    --read-only
    --cap-drop ALL
    --security-opt no-new-privileges:true
    --network "$NETWORK"
)
cpu_id="$(taskset -c "$management_cpuset" sudo -n docker run \
    "${common_run[@]}" --name "$CPU_CONTAINER" "$IMAGE" cpu "$evaluation_cpuset")"
memory_id="$(taskset -c "$management_cpuset" sudo -n docker run \
    "${common_run[@]}" --name "$MEMORY_CONTAINER" "$IMAGE" memory)"

for _ in $(seq 1 100); do
    cpu_ready="$(sudo -n docker logs "$cpu_id" 2>/dev/null || true)"
    memory_ready="$(sudo -n docker logs "$memory_id" 2>/dev/null || true)"
    [ "$cpu_ready" = cpu-bomb-ready ] && [ "$memory_ready" = memory-bomb-ready ] && break
    sleep 0.05
done
[ "${cpu_ready:-}" = cpu-bomb-ready ] || die "CPU bomb did not become ready"
[ "${memory_ready:-}" = memory-bomb-ready ] || die "memory bomb did not become ready"

cpu_pid="$(sudo -n docker inspect --format '{{.State.Pid}}' "$cpu_id")"
declare -A observed_cpus=()
IFS=, read -r -a evaluation_cpus <<<"$evaluation_cpuset"
for _ in $(seq 1 100); do
    while IFS= read -r observed_cpu; do
        [[ "$observed_cpu" =~ ^[0-9]+$ ]] && observed_cpus[$observed_cpu]=1
    done < <(sudo -n awk '{print $39}' "/proc/$cpu_pid"/task/*/stat)
    all_evaluation_cpus_observed=true
    for cpu in "${evaluation_cpus[@]}"; do
        if [ -z "${observed_cpus[$cpu]:-}" ]; then
            all_evaluation_cpus_observed=false
            break
        fi
    done
    [ "$all_evaluation_cpus_observed" = true ] && break
    sleep 0.02
done
[ "${all_evaluation_cpus_observed:-false}" = true ] ||
    die "CPU bomb did not execute on every evaluation CPU"
observed_cpuset="$(for cpu in "${!observed_cpus[@]}"; do printf '%s\n' "$cpu"; done | sort -n | paste -sd, -)"
[ "$observed_cpuset" = "$evaluation_cpuset" ] ||
    die "CPU bomb executed outside the evaluation CPU set: $observed_cpuset"

memory_exit_code="$(timeout --signal=TERM --kill-after=2s "${memory_wait_seconds}s" \
    taskset -c "$management_cpuset" sudo -n docker wait "$memory_id")" ||
    die "memory bomb did not hit its hard limit"
[ "$memory_exit_code" -eq 137 ] || die "memory bomb exited $memory_exit_code instead of 137"
[ "$(sudo -n docker inspect --format '{{.State.OOMKilled}}' "$memory_id")" = true ] ||
    die "memory bomb was not marked OOM-killed"
[ "$(sudo -n docker inspect --format '{{.HostConfig.CpusetCpus}}' "$cpu_id")" = "$evaluation_cpuset" ] ||
    die "CPU bomb escaped the evaluation CPU set"
[ "$(sudo -n docker inspect --format '{{.HostConfig.Memory}}' "$memory_id")" = "$memory_bomb_limit" ] ||
    die "memory bomb limit changed"
[ "$(sudo -n docker inspect --format '{{.State.Running}}' "$cpu_id")" = true ] ||
    die "CPU bomb stopped before cleanup"

cleanup_start_ms="$(date +%s%3N)"
cleanup_job_objects
cleanup_end_ms="$(date +%s%3N)"
cleanup_elapsed_ms=$((cleanup_end_ms - cleanup_start_ms))
[ "$cleanup_elapsed_ms" -le "$CLEANUP_LIMIT_MS" ] ||
    die "management cleanup took ${cleanup_elapsed_ms}ms"
[ -z "$(sudo -n docker ps -aq --filter "label=com.urnetwork.competition.job-id=$JOB_ID")" ] ||
    die "cleanup left labeled containers"
[ -z "$(sudo -n docker network ls -q --filter "label=com.urnetwork.competition.job-id=$JOB_ID")" ] ||
    die "cleanup left labeled networks"

jq -n \
    --arg image "$IMAGE" \
    --arg evaluation_cpuset "$evaluation_cpuset" \
    --arg cpu_bomb_observed_cpuset "$observed_cpuset" \
    --arg management_cpuset "$management_cpuset" \
    --arg management_affinity "$actual_affinity" \
    --argjson memory_limit_bytes "$memory_bomb_limit" \
    --argjson memory_exit_code "$memory_exit_code" \
    --argjson production_memory_limit "$production_memory_limit" \
    --argjson cleanup_elapsed_ms "$cleanup_elapsed_ms" \
    --argjson cleanup_limit_ms "$CLEANUP_LIMIT_MS" \
    '{schema:1,kind:"sim-latency-resource-bomb-cleanup",
      image:$image,evaluation_cpuset:$evaluation_cpuset,
      management_cpuset:$management_cpuset,management_affinity:$management_affinity,
      cpu_bomb_observed_cpuset:$cpu_bomb_observed_cpuset,
      cpu_bomb_saturated_evaluation_set:true,memory_limit_bytes:$memory_limit_bytes,
      production_memory_limit:$production_memory_limit,
      memory_exit_code:$memory_exit_code,memory_oom_killed:true,
      cleanup_elapsed_ms:$cleanup_elapsed_ms,cleanup_limit_ms:$cleanup_limit_ms,
      cleanup_complete:true,residual_containers:0,residual_networks:0}'
