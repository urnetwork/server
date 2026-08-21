#!/usr/bin/env bash

# Apply or verify the reversible runtime CPU/memory controls for the single
# authoritative evaluator host. IRQ placement, Docker user namespaces, and the
# host firewall are qualified separately because they require deployment-level
# policy and, for Docker, a controlled daemon restart.

set -Eeuo pipefail
umask 077

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
readonly RESOURCE_BOUNDARY="$SCRIPT_DIR/container/resource-boundary.sh"

mode="${1:---check}"
[ "$#" -eq 1 ] || {
    printf 'usage: authoritative-host-controls --check|--apply\n' >&2
    exit 2
}
case "$mode" in
    --check|--apply) ;;
    *)
        printf 'usage: authoritative-host-controls --check|--apply\n' >&2
        exit 2
        ;;
esac

die() {
    printf '[competition-host-controls] ERROR: %s\n' "$*" >&2
    exit 1
}

for command in awk jq lscpu paste sort sysctl; do
    command -v "$command" >/dev/null 2>&1 || die "required command missing: $command"
done
[ -x "$RESOURCE_BOUNDARY" ] || die "resource-boundary helper is unavailable"

write_root_file() {
    local path="$1" value="$2"
    [ -e "$path" ] || die "control path is absent: $path"
    printf '%s\n' "$value" | sudo -n tee "$path" >/dev/null ||
        die "could not write control path: $path"
}

if [ "$mode" = --apply ]; then
    [ "$(id -u)" -eq 0 ] || command -v sudo >/dev/null 2>&1 || die "sudo is required"
    if [ -w /sys/devices/system/cpu/smt/control ]; then
        printf 'off\n' > /sys/devices/system/cpu/smt/control
    else
        write_root_file /sys/devices/system/cpu/smt/control off
    fi

    while IFS= read -r cpu; do
        governor_path="/sys/devices/system/cpu/cpu$cpu/cpufreq/scaling_governor"
        [ -e "$governor_path" ] || die "CPU $cpu has no governor control"
        if [ -w "$governor_path" ]; then
            printf 'performance\n' > "$governor_path"
        else
            write_root_file "$governor_path" performance
        fi
    done < <(lscpu -p=CPU | awk -F, '!/^#/ {print $1}')

    if [ -e /sys/devices/system/cpu/intel_pstate/no_turbo ]; then
        if [ -w /sys/devices/system/cpu/intel_pstate/no_turbo ]; then
            printf '1\n' > /sys/devices/system/cpu/intel_pstate/no_turbo
        else
            write_root_file /sys/devices/system/cpu/intel_pstate/no_turbo 1
        fi
    elif [ -e /sys/devices/system/cpu/cpufreq/boost ]; then
        if [ -w /sys/devices/system/cpu/cpufreq/boost ]; then
            printf '0\n' > /sys/devices/system/cpu/cpufreq/boost
        else
            write_root_file /sys/devices/system/cpu/cpufreq/boost 0
        fi
    else
        die "turbo control is unavailable"
    fi
    sudo -n sysctl -q -w vm.overcommit_memory=1 >/dev/null || die "could not set vm.overcommit_memory"
fi

host_cpu_list="$(lscpu -p=CPU | awk -F, '!/^#/ {print $1}' | paste -sd, -)"
logical_cpu_count="$(lscpu -p=CPU | awk -F, '!/^#/ {count++} END {print count+0}')"
physical_core_count="$(lscpu -p=SOCKET,CORE | awk -F, '!/^#/ {seen[$1 ":" $2]=1} END {print length(seen)+0}')"
threads_per_core="$(lscpu -p=CPU,CORE | awk -F, '!/^#/ {count[$2]++} END {max=0; for (core in count) if (max < count[core]) max=count[core]; print max+0}')"
smt_control="$(tr -d '\n' </sys/devices/system/cpu/smt/control 2>/dev/null || true)"
governors="$(for path in /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor; do
    [ -r "$path" ] || continue
    cpu="${path#/sys/devices/system/cpu/cpu}"
    cpu="${cpu%%/*}"
    [ ! -r "/sys/devices/system/cpu/cpu$cpu/online" ] ||
        [ "$(<"/sys/devices/system/cpu/cpu$cpu/online")" = 1 ] || continue
    tr -d '\n' <"$path"
    printf '\n'
done | sort -u | paste -sd, -)"

turbo_state=unknown
if [ -r /sys/devices/system/cpu/intel_pstate/no_turbo ]; then
    [ "$(tr -d '\n' </sys/devices/system/cpu/intel_pstate/no_turbo)" = 1 ] &&
        turbo_state=disabled || turbo_state=enabled
elif [ -r /sys/devices/system/cpu/cpufreq/boost ]; then
    [ "$(tr -d '\n' </sys/devices/system/cpu/cpufreq/boost)" = 0 ] &&
        turbo_state=disabled || turbo_state=enabled
fi
overcommit_memory="$(sysctl -n vm.overcommit_memory)"
boundary="$($RESOURCE_BOUNDARY)"

passed=false
if [ "$logical_cpu_count" -eq 12 ] && [ "$physical_core_count" -eq 12 ] &&
   [ "$threads_per_core" -eq 1 ] &&
   { [ "$smt_control" = off ] || [ "$smt_control" = forceoff ] || [ "$smt_control" = notsupported ]; } &&
   [ "$governors" = performance ] && [ "$turbo_state" = disabled ] &&
   [ "$overcommit_memory" -eq 1 ] &&
   jq -e '.evaluation_physical_core_count == 10 and .management_physical_core_count == 2 and
           .management_logical_cpu_count == 2 and .disjoint_cpu_sets == true and
           .memory_capacity_passed == true' <<<"$boundary" >/dev/null; then
    passed=true
fi

jq -n \
    --arg mode "${mode#--}" --arg host_cpu_list "$host_cpu_list" \
    --arg smt_control "$smt_control" --arg governors "$governors" \
    --arg turbo_state "$turbo_state" \
    --argjson logical_cpu_count "$logical_cpu_count" \
    --argjson physical_core_count "$physical_core_count" \
    --argjson threads_per_core "$threads_per_core" \
    --argjson overcommit_memory "$overcommit_memory" \
    --argjson boundary "$boundary" --argjson passed "$passed" \
    '{schema:1,kind:"sim-latency-authoritative-host-controls",mode:$mode,
      passed:$passed,host_cpu_list:$host_cpu_list,
      logical_cpu_count:$logical_cpu_count,physical_core_count:$physical_core_count,
      threads_per_core:$threads_per_core,smt_control:$smt_control,
      governors:$governors,turbo_state:$turbo_state,
      overcommit_memory:$overcommit_memory,
      evaluation_cpuset:$boundary.evaluation_cpuset,
      management_cpuset:$boundary.management_cpuset,
      management_logical_cpu_count:$boundary.management_logical_cpu_count,
      capacity_reserve_bytes:$boundary.capacity_reserve_bytes}'

[ "$passed" = true ]
