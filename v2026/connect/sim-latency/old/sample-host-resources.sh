#!/usr/bin/env bash
#
# sample-host-resources.sh -- low-overhead host telemetry for a local baseline.
#
# Usage: sample-host-resources.sh <out.csv> [interval_s] [stop_pid]
#
# CPU is the sum of ps %CPU for every sim-latency process. Linux reports 100%
# per fully occupied logical CPU, so divide by the exposed CPU count when a
# host-normalized fraction is needed. stop_pid is normally the detached
# campaign supervisor; sampling ends after that process exits.

set -u -o pipefail

out="${1:?usage: sample-host-resources.sh <out.csv> [interval_s] [stop_pid]}"
interval="${2:-30}"
stop_pid="${3:-}"

if [ ! -s "$out" ]; then
    printf '%s\n' 'unix_s,sim_processes,sim_cpu_pct,sim_rss_kb,load1,mem_available_kb,swap_used_kb,tcp_established' >> "$out"
fi

while true; do
    if [ -n "$stop_pid" ] && ! kill -0 "$stop_pid" 2>/dev/null; then
        exit 0
    fi

    now=$(date +%s)
    read -r sim_processes sim_cpu sim_rss < <(
        LC_ALL=C ps axo pcpu=,rss=,comm= | awk '
            $3 == "sim-latency" { n += 1; cpu += $1; rss += $2 }
            END { printf "%d %.1f %d\n", n, cpu, rss }
        '
    )
    read -r load1 _ < /proc/loadavg
    mem_available=$(awk '$1 == "MemAvailable:" { print $2 }' /proc/meminfo)
    swap_used=$(awk '
        $1 == "SwapTotal:" { total = $2 }
        $1 == "SwapFree:" { free = $2 }
        END { print total - free }
    ' /proc/meminfo)
    tcp_established=$(ss -Htan state established 2>/dev/null | wc -l)

    printf '%s,%s,%s,%s,%s,%s,%s,%s\n' \
        "$now" "${sim_processes:-0}" "${sim_cpu:-0}" "${sim_rss:-0}" \
        "${load1:-0}" "${mem_available:-0}" "${swap_used:-0}" \
        "${tcp_established:-0}" >> "$out"

    sleep "$interval"
done
