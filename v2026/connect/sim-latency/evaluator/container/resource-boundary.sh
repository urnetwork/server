#!/usr/bin/env bash

# Derive the fixed evaluator/management split from live CPU topology and prove
# that the frozen container ceilings leave a hard host-memory reserve.

set -Eeuo pipefail

readonly EVALUATION_PHYSICAL_CORE_COUNT=10
readonly MANAGEMENT_PHYSICAL_CORE_COUNT=2
readonly RUNNER_MEMORY_LIMIT=72g
readonly RUNNER_MEMORY_BYTES=77309411328
readonly POSTGRES_MEMORY_LIMIT=16g
readonly POSTGRES_MEMORY_BYTES=17179869184
readonly REDIS_MEMORY_LIMIT=8g
readonly REDIS_MEMORY_BYTES=8589934592
readonly BUILD_MEMORY_LIMIT=12g
readonly BUILD_MEMORY_BYTES=12884901888
readonly EVIDENCE_MEMORY_LIMIT=4g
readonly EVIDENCE_MEMORY_BYTES=4294967296
readonly MINIMUM_MANAGEMENT_MEMORY_RESERVE_BYTES=25769803776

die() { printf '[competition-resource-boundary] ERROR: %s\n' "$*" >&2; exit 1; }

# Evidence survives container teardown, and host copies are not charged to the
# next runner. Budget its entire cap separately, even when some pages are also
# charged to an active container. This deliberately errs on the safe side.
memory_budget() {
    local host_memory_bytes="$1"
    [[ "$host_memory_bytes" =~ ^[1-9][0-9]{0,14}$ ]] || die "host memory identity is invalid"
    local active_memory_limit_bytes=$((RUNNER_MEMORY_BYTES + POSTGRES_MEMORY_BYTES + REDIS_MEMORY_BYTES))
    local total_evaluation_memory_limit_bytes=$((active_memory_limit_bytes + EVIDENCE_MEMORY_BYTES))
    local capacity_reserve_bytes=$((host_memory_bytes - total_evaluation_memory_limit_bytes))
    [ "$capacity_reserve_bytes" -ge "$MINIMUM_MANAGEMENT_MEMORY_RESERVE_BYTES" ] ||
        die "container and evidence ceilings leave only $capacity_reserve_bytes bytes for host management"
    jq -n \
        --arg evidence_memory_limit "$EVIDENCE_MEMORY_LIMIT" \
        --argjson host_memory_bytes "$host_memory_bytes" \
        --argjson active_memory_limit_bytes "$active_memory_limit_bytes" \
        --argjson evidence_memory_limit_bytes "$EVIDENCE_MEMORY_BYTES" \
        --argjson total_evaluation_memory_limit_bytes "$total_evaluation_memory_limit_bytes" \
        --argjson minimum_management_memory_reserve_bytes "$MINIMUM_MANAGEMENT_MEMORY_RESERVE_BYTES" \
        --argjson capacity_reserve_bytes "$capacity_reserve_bytes" \
        '{host_memory_bytes:$host_memory_bytes,active_memory_limit_bytes:$active_memory_limit_bytes,
          evidence_memory_limit:$evidence_memory_limit,evidence_memory_limit_bytes:$evidence_memory_limit_bytes,
          total_evaluation_memory_limit_bytes:$total_evaluation_memory_limit_bytes,
          minimum_management_memory_reserve_bytes:$minimum_management_memory_reserve_bytes,
          capacity_reserve_bytes:$capacity_reserve_bytes,memory_capacity_passed:true}'
}

# Read-only policy arithmetic for qualification and deterministic boundary tests.
# It intentionally emits no CPU/host attestation and cannot qualify a host.
if [ "$#" -ne 0 ]; then
    [ "$#" -eq 2 ] && [ "$1" = --memory-budget ] || die 'usage: resource-boundary.sh [--memory-budget HOST_BYTES]'
    memory_budget "$2"
    exit 0
fi

for command in awk find jq sed sort; do
    command -v "$command" >/dev/null 2>&1 || die "required command missing: $command"
done

declare -A seen_cores=()
declare -A first_core_cpus=()
declare -A all_core_cpus=()
core_keys=()
while IFS= read -r cpu; do
    topology_path="/sys/devices/system/cpu/cpu$cpu"
    [ ! -r "$topology_path/online" ] || [ "$(<"$topology_path/online")" = 1 ] || continue
    package="$(<"$topology_path/topology/physical_package_id")"
    core="$(<"$topology_path/topology/core_id")"
    core_key="$package:$core"
    if [ -z "${seen_cores[$core_key]:-}" ]; then
        seen_cores[$core_key]=1
        core_keys+=("$core_key")
        first_core_cpus[$core_key]="$cpu"
        all_core_cpus[$core_key]="$cpu"
    else
        all_core_cpus[$core_key]="${all_core_cpus[$core_key]},$cpu"
    fi
done < <(find /sys/devices/system/cpu -maxdepth 1 -type d -name 'cpu[0-9]*' -printf '%f\n' |
    sed 's/^cpu//' | sort -n)

required_core_count=$((EVALUATION_PHYSICAL_CORE_COUNT + MANAGEMENT_PHYSICAL_CORE_COUNT))
[ "${#core_keys[@]}" -ge "$required_core_count" ] ||
    die "host exposes ${#core_keys[@]} physical cores; $required_core_count are required"

evaluation_cpus=()
for ((index = 0; index < EVALUATION_PHYSICAL_CORE_COUNT; index++)); do
    evaluation_cpus+=("${first_core_cpus[${core_keys[$index]}]}")
done
management_cpus=()
for ((index = EVALUATION_PHYSICAL_CORE_COUNT; index < required_core_count; index++)); do
    IFS=, read -r -a sibling_cpus <<<"${all_core_cpus[${core_keys[$index]}]}"
    management_cpus+=("${sibling_cpus[@]}")
done

evaluation_cpuset="$(IFS=,; printf '%s' "${evaluation_cpus[*]}")"
management_cpuset="$(IFS=,; printf '%s' "${management_cpus[*]}")"
[ -n "$evaluation_cpuset" ] && [ -n "$management_cpuset" ] || die "derived CPU sets are empty"

declare -A evaluation_cpu_set=()
for cpu in "${evaluation_cpus[@]}"; do evaluation_cpu_set[$cpu]=1; done
for cpu in "${management_cpus[@]}"; do
    [ -z "${evaluation_cpu_set[$cpu]:-}" ] || die "evaluation and management CPU sets overlap at CPU $cpu"
done

host_memory_bytes="$(awk '/^MemTotal:/ {printf "%.0f", $2 * 1024; exit}' /proc/meminfo)"
memory_budget_json="$(memory_budget "$host_memory_bytes")" || exit 1

jq -n \
    --arg evaluation_cpuset "$evaluation_cpuset" \
    --arg management_cpuset "$management_cpuset" \
    --arg runner_memory_limit "$RUNNER_MEMORY_LIMIT" \
    --arg postgres_memory_limit "$POSTGRES_MEMORY_LIMIT" \
    --arg redis_memory_limit "$REDIS_MEMORY_LIMIT" \
    --arg build_memory_limit "$BUILD_MEMORY_LIMIT" \
    --argjson discovered_physical_core_count "${#core_keys[@]}" \
    --argjson selected_physical_core_count "$required_core_count" \
    --argjson evaluation_physical_core_count "$EVALUATION_PHYSICAL_CORE_COUNT" \
    --argjson management_physical_core_count "$MANAGEMENT_PHYSICAL_CORE_COUNT" \
    --argjson management_logical_cpu_count "${#management_cpus[@]}" \
    --argjson memory_budget "$memory_budget_json" \
    --argjson runner_memory_limit_bytes "$RUNNER_MEMORY_BYTES" \
    --argjson postgres_memory_limit_bytes "$POSTGRES_MEMORY_BYTES" \
    --argjson redis_memory_limit_bytes "$REDIS_MEMORY_BYTES" \
    --argjson build_memory_limit_bytes "$BUILD_MEMORY_BYTES" \
    '{schema:1,kind:"sim-latency-resource-boundary",
      evaluation_cpuset:$evaluation_cpuset,management_cpuset:$management_cpuset,
      discovered_physical_core_count:$discovered_physical_core_count,
      selected_physical_core_count:$selected_physical_core_count,
      evaluation_physical_core_count:$evaluation_physical_core_count,
      management_physical_core_count:$management_physical_core_count,
      management_logical_cpu_count:$management_logical_cpu_count,
      runner_memory_limit:$runner_memory_limit,
      runner_memory_limit_bytes:$runner_memory_limit_bytes,
      postgres_memory_limit:$postgres_memory_limit,
      postgres_memory_limit_bytes:$postgres_memory_limit_bytes,
      redis_memory_limit:$redis_memory_limit,
      redis_memory_limit_bytes:$redis_memory_limit_bytes,
      build_memory_limit:$build_memory_limit,
      build_memory_limit_bytes:$build_memory_limit_bytes,
      disjoint_cpu_sets:true} + $memory_budget'
