#!/usr/bin/env bash
#
# Low-overhead observational telemetry for native PostgreSQL and Redis
# processes during a local baseline. RSS is summed across matching processes;
# PostgreSQL therefore includes shared pages once per backend and is a trend
# diagnostic, not a cgroup or unique-memory measurement.
#
# Usage: sample-service-resources.sh <out.csv> [interval_s] [stop_pid]

set -u -o pipefail

out="${1:?usage: sample-service-resources.sh <out.csv> [interval_s] [stop_pid]}"
interval="${2:-30}"
stop_pid="${3:-}"
header='unix_s,postgres_processes,postgres_summed_rss_kb,redis_processes,redis_summed_rss_kb'

if [ ! -s "$out" ]; then
    printf '%s\n' "$header" >> "$out"
elif [ "$(sed -n '1p' "$out")" != "$header" ]; then
    printf 'service telemetry header mismatch: %s\n' "$out" >&2
    exit 1
fi

while true; do
    if [ -n "$stop_pid" ] && ! kill -0 "$stop_pid" 2>/dev/null; then
        exit 0
    fi

    now=$(date +%s)
    read -r sim_processes postgres_processes postgres_rss redis_processes redis_rss < <(
        LC_ALL=C ps axo comm=,rss= | awk '
            $1 == "sim-latency" { sim_n += 1 }
            $1 == "postgres" { pg_n += 1; pg_rss += $2 }
            $1 == "redis-server" { redis_n += 1; redis_rss += $2 }
            END { printf "%d %d %d %d %d\n", sim_n, pg_n, pg_rss, redis_n, redis_rss }
        '
    )
    # Only append while the frozen local topology (one runner plus four fleet
    # shards) exists. This keeps the telemetry file byte-stable while the
    # finalizer generates and deterministically replays the summary.
    if [ "${sim_processes:-0}" -ne 5 ]; then
        sleep "$interval"
        continue
    fi
    printf '%s,%s,%s,%s,%s\n' \
        "$now" "${postgres_processes:-0}" "${postgres_rss:-0}" \
        "${redis_processes:-0}" "${redis_rss:-0}" >> "$out"

    sleep "$interval"
done
