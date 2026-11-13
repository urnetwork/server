#!/usr/bin/env bash
#
# sample-rss.sh -- sample the memory footprint of the whole evaluation stack
# every INTERVAL seconds, appending CSV rows to OUT:
#
#   unix_s,sim_rss_kb,docker_rss_kb,pg_mem_mb,redis_mem_mb
#
#   sim_rss_kb     host RSS summed over every sim-latency process (run + fleet shards)
#   docker_rss_kb  host RSS summed over the Docker Desktop VM/backend processes
#   pg_mem_mb      container memory usage reported by docker stats
#   redis_mem_mb   container memory usage reported by docker stats
#
# Usage: sample-rss.sh <out.csv> [interval_s]

out="${1:?usage: sample-rss.sh <out.csv> [interval_s]}"
interval="${2:-15}"

if [ ! -s "$out" ]; then
    echo "unix_s,sim_rss_kb,docker_rss_kb,pg_mem_mb,redis_mem_mb" >> "$out"
fi

mem_mb() {
    # "123.4MiB / 30GiB" -> 123.4 ; "1.2GiB / ..." -> 1228.8
    case "$1" in
        *GiB*) echo "$1" | awk '{gsub(/GiB.*/,"",$1); printf "%.1f", $1*1024}' ;;
        *MiB*) echo "$1" | awk '{gsub(/MiB.*/,"",$1); printf "%.1f", $1}' ;;
        *KiB*) echo "$1" | awk '{gsub(/KiB.*/,"",$1); printf "%.1f", $1/1024}' ;;
        *) echo "0" ;;
    esac
}

while true; do
    now=$(date +%s)
    sim_rss=$(ps axo rss,comm | awk '/sim-latency/ {sum+=$1} END {printf "%d", sum}')
    docker_rss=$(ps axo rss,comm | awk '/[cC]om\.docker|[dD]ocker/ {sum+=$1} END {printf "%d", sum}')
    stats=$(docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' urnetwork-local-pg urnetwork-local-redis 2>/dev/null)
    pg_mem=$(mem_mb "$(echo "$stats" | awk '/urnetwork-local-pg/ {print $2}')")
    redis_mem=$(mem_mb "$(echo "$stats" | awk '/urnetwork-local-redis/ {print $2}')")
    echo "$now,${sim_rss:-0},${docker_rss:-0},${pg_mem:-0},${redis_mem:-0}" >> "$out"
    sleep "$interval"
done
