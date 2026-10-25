#!/usr/bin/env bash
# Reproducible production-candidate frontier runner for this host class.
#
# This is qualification tooling, not the production runner. It constrains the
# simulator and all fleet children to one hardware thread from each of this
# Xeon's 12 physical cores. PostgreSQL and Redis are still host services and
# therefore remain outside this unprivileged job boundary; summaries always
# record production_qualified=false until the real worker contains them too.
#
# Usage:
#   ./eval-frontier-12c.sh hardware
#   ./eval-frontier-12c.sh init <profile> <providers> <clients> <rate/min> [seed] [quality-window] [hosts] [fleet-shards]
#   ./eval-frontier-12c.sh run <profile> <run-tag> [duration] [impair|no-impair]
#   ./eval-frontier-12c.sh summarize <profile> <run-tag>

set -Eeuo pipefail
umask 077

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
readonly ARTIFACT_ROOT="$SCRIPT_DIR/eval-12c"
# Keep the authenticated local-baseline binary at ./sim-latency untouched.
# A corrected or candidate build can be selected explicitly; its digest is
# frozen into the profile and rechecked before every run.
readonly SIM_BIN="${SIM_LATENCY_BIN:-$SCRIPT_DIR/sim-latency}"
readonly CPUSET="0,2,4,6,8,10,12,14,16,18,20,22"
readonly CPU_COUNT=12
readonly CPU_MEAN_LIMIT_CORES="7.8"
readonly MEMORY_AVAILABLE_MIN_GIB="25.0"
readonly HOST_SAMPLE_INTERVAL=5
readonly G5_LOG_PATTERN='Unexpected error:|Rescue handler panic|client driver panic:|evaluation panic:|http: panic serving|panic recovered|(^|[[:space:]])panic:|fatal error:|runtime: out of memory|out of memory: killed process|service restart|restarting service|service unavailable|service missing|unclean drain|did not drain within'

log() { printf '[frontier-12c] %s %s\n' "$(date -u '+%F %T UTC')" "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }

require_command() {
    command -v "$1" >/dev/null 2>&1 || die "required command missing: $1"
}

require_name() {
    [[ "$1" =~ ^[a-z0-9][a-z0-9._-]{0,63}$ ]] ||
        die "name must match [a-z0-9][a-z0-9._-]{0,63}: $1"
}

require_positive_integer() {
    [[ "$2" =~ ^[1-9][0-9]*$ ]] || die "$1 must be a positive integer: $2"
}

require_nonnegative_integer() {
    [[ "$2" =~ ^[0-9]+$ ]] || die "$1 must be a non-negative integer: $2"
}

sha256_file() { sha256sum "$1" | awk '{print $1}'; }

hardware_json() {
    require_command jq
    require_command lscpu
    require_command taskset

    local model sockets cores_per_socket threads_per_core logical affinity_count
    local mem_kib governor turbo smt kernel os_version
    model="$(lscpu | awk -F: '$1 ~ /^Model name/ {sub(/^[[:space:]]+/, "", $2); print $2}')"
    sockets="$(lscpu | awk -F: '$1 ~ /^Socket\(s\)/ {gsub(/[[:space:]]/, "", $2); print $2}')"
    cores_per_socket="$(lscpu | awk -F: '$1 ~ /^Core\(s\) per socket/ {gsub(/[[:space:]]/, "", $2); print $2}')"
    threads_per_core="$(lscpu | awk -F: '$1 ~ /^Thread\(s\) per core/ {gsub(/[[:space:]]/, "", $2); print $2}')"
    logical="$(nproc --all)"
    affinity_count="$(taskset -c "$CPUSET" nproc)"
    mem_kib="$(awk '$1 == "MemTotal:" {print $2}' /proc/meminfo)"
    governor="$(cat /sys/devices/system/cpu/cpu0/cpufreq/scaling_governor 2>/dev/null || printf unknown)"
    if [ -r /sys/devices/system/cpu/intel_pstate/no_turbo ]; then
        turbo="$([ "$(cat /sys/devices/system/cpu/intel_pstate/no_turbo)" = 0 ] && printf enabled || printf disabled)"
    else
        turbo=unknown
    fi
    smt="$(cat /sys/devices/system/cpu/smt/control 2>/dev/null || printf unknown)"
    kernel="$(uname -r)"
    # shellcheck disable=SC1091
    . /etc/os-release
    os_version="${ID:-unknown}-${VERSION_ID:-unknown}"

    jq -n \
        --arg hostname "$(hostname)" \
        --arg model "$model" \
        --arg cpuset "$CPUSET" \
        --arg governor "$governor" \
        --arg turbo "$turbo" \
        --arg smt "$smt" \
        --arg kernel "$kernel" \
        --arg os_version "$os_version" \
        --argjson sockets "${sockets:-0}" \
        --argjson cores_per_socket "${cores_per_socket:-0}" \
        --argjson threads_per_core "${threads_per_core:-0}" \
        --argjson logical_cpus "${logical:-0}" \
        --argjson affinity_cpus "${affinity_count:-0}" \
        --argjson mem_total_kib "${mem_kib:-0}" \
        '{
            schema: 1,
            kind: "sim-latency-frontier-hardware",
            hostname: $hostname,
            cpu_model: $model,
            sockets: $sockets,
            cores_per_socket: $cores_per_socket,
            threads_per_core: $threads_per_core,
            logical_cpus_online: $logical_cpus,
            candidate_cpuset: $cpuset,
            candidate_affinity_cpus: $affinity_cpus,
            memory_total_kib: $mem_total_kib,
            os_version: $os_version,
            kernel: $kernel,
            smt_control: $smt,
            governor: $governor,
            turbo: $turbo,
            simulator_children_affinity_constrained: true,
            postgres_redis_inside_job_boundary: false,
            production_qualified: false
        }'
}

hardware_gate() {
    local hardware
    hardware="$(hardware_json)"
    jq -e '
        .sockets == 1 and
        .cores_per_socket == 12 and
        .candidate_affinity_cpus == 12 and
        .memory_total_kib >= 120000000 and
        .os_version == "ubuntu-24.04"
    ' <<<"$hardware" >/dev/null || die "host does not meet the production-candidate hardware floor"
}

profile_dir() { printf '%s/profiles/%s' "$ARTIFACT_ROOT" "$1"; }
run_dir() { printf '%s/runs/%s' "$(profile_dir "$1")" "$2"; }

init_profile() {
    local profile="${1:?missing profile name}"
    local providers="${2:?missing provider count}"
    local clients="${3:?missing client count}"
    local rate="${4:?missing arrival rate}"
    local seed="${5:-48}"
    local quality_window="${6:-0}"
    local hosts="${7:-4}"
    local fleet_shards="${8:-4}"
    require_name "$profile"
    require_positive_integer providers "$providers"
    require_positive_integer clients "$clients"
    require_positive_integer rate "$rate"
    require_positive_integer seed "$seed"
    require_nonnegative_integer quality-window "$quality_window"
    [ "$quality_window" -le 32 ] || die "quality-window must be in 0..32"
    require_positive_integer hosts "$hosts"
    require_nonnegative_integer fleet-shards "$fleet_shards"
    hardware_gate
    [ -x "$SIM_BIN" ] || die "frozen simulator binary is missing: $SIM_BIN"

    local dir config profile_json
    dir="$(profile_dir "$profile")"
    config="$dir/providers.yml"
    profile_json="$dir/profile.json"
    [ ! -e "$dir" ] || die "profile already exists: $dir"
    mkdir -p "$dir/runs"

    log "generating profile=$profile providers=$providers clients=$clients rate=$rate seed=$seed quality_window=$quality_window hosts=$hosts fleet_shards=$fleet_shards"
    local init_args=(
        init
        --count "$providers" \
        --clients "$clients" \
        --rate "$rate" \
        --seed "$seed" \
        --quality-window "$quality_window" \
        --out "$config"
    )
    "$SIM_BIN" "${init_args[@]}"

    local config_sha binary_sha hardware
    config_sha="$(sha256_file "$config")"
    binary_sha="$(sha256_file "$SIM_BIN")"
    hardware="$(hardware_json)"
    jq -n \
        --arg profile "$profile" \
        --arg config "$config" \
        --arg config_sha256 "$config_sha" \
        --arg binary_sha256 "$binary_sha" \
        --arg cpuset "$CPUSET" \
        --argjson providers "$providers" \
        --argjson clients "$clients" \
        --argjson rate_per_minute "$rate" \
        --argjson seed "$seed" \
        --argjson quality_window_size "$quality_window" \
        --argjson hosts "$hosts" \
        --argjson fleet_shards "$fleet_shards" \
        --argjson hardware "$hardware" \
        '{
            schema: 1,
            kind: "sim-latency-frontier-profile",
            profile: $profile,
            providers: $providers,
            clients: $clients,
            rate_per_minute: $rate_per_minute,
            seed: $seed,
            quality_window_size: $quality_window_size,
            hosts: $hosts,
            fleet_shards: $fleet_shards,
            providers_path: $config,
            providers_sha256: $config_sha256,
            simulator_sha256: $binary_sha256,
            cpuset: $cpuset,
            gomaxprocs: 12,
            hardware: $hardware,
            classification: "production_candidate_unprivileged"
        }' >"$profile_json"
    log "profile ready: $profile_json config_sha256=$config_sha"
}

ensure_sample_auditor() {
    local auditor="$ARTIFACT_ROOT/tools/baseline-samples"
    if [ ! -x "$auditor" ]; then
        mkdir -p "$(dirname "$auditor")"
        log "building post-run sample auditor"
        (
            cd "$SCRIPT_DIR"
            GOTOOLCHAIN=go1.26.5 go build -o "$auditor" ./cmd/baseline-samples
        )
    fi
    printf '%s' "$auditor"
}

summarize_run() {
    local profile="${1:?missing profile name}"
    local tag="${2:?missing run tag}"
    require_name "$profile"
    require_name "$tag"
    local pdir rdir profile_json meta csv log_path host_csv service_csv samples_json output
    pdir="$(profile_dir "$profile")"
    rdir="$(run_dir "$profile" "$tag")"
    profile_json="$pdir/profile.json"
    meta="$rdir/run.json"
    csv="$rdir/results.csv"
    log_path="$rdir/stderr.log"
    host_csv="$rdir/host.csv"
    service_csv="$rdir/services.csv"
    samples_json="$rdir/samples.json"
    output="$rdir/summary.json"
    for required in "$profile_json" "$meta" "$csv" "$log_path" "$host_csv" "$service_csv" "$samples_json"; do
        [ -f "$required" ] || die "missing run artifact: $required"
    done

    python3 -B - \
        "$profile_json" "$meta" "$csv" "$log_path" "$host_csv" \
        "$service_csv" "$samples_json" "$CPU_MEAN_LIMIT_CORES" \
        "$MEMORY_AVAILABLE_MIN_GIB" >"$output" <<'PY'
import csv
import hashlib
import json
import math
import re
import statistics
import sys
from pathlib import Path

paths = [Path(value) for value in sys.argv[1:8]]
(
    profile_path,
    meta_path,
    csv_path,
    log_path,
    host_path,
    service_path,
    samples_path,
) = paths
cpu_limit_text, mem_available_limit_text = sys.argv[8:10]
cpu_limit = float(cpu_limit_text)
mem_available_limit = float(mem_available_limit_text)

profile = json.loads(profile_path.read_text())
meta_bytes = meta_path.read_bytes()
meta = json.loads(meta_bytes)
samples = json.loads(samples_path.read_text())
start_ms = int(meta["measure_start_ms"])
end_ms = int(meta["measure_end_ms"])
duration_s = (end_ms - start_ms) / 1000.0

def quantile_type7(values, q):
    values = sorted(values)
    if not values:
        raise ValueError("empty quantile input")
    h = q * (len(values) - 1)
    lo = math.floor(h)
    hi = math.ceil(h)
    return values[lo] if lo == hi else values[lo] + (h - lo) * (values[hi] - values[lo])

observations = []
rows = 0
successes = 0
received_bytes = 0
csv_hasher = hashlib.sha256()
with csv_path.open("rb") as raw:
    for chunk in iter(lambda: raw.read(1024 * 1024), b""):
        csv_hasher.update(chunk)
with csv_path.open(newline="") as handle:
    reader = csv.DictReader(handle)
    for row in reader:
        t_start = int(row["t_start_ms"])
        if t_start < start_ms or end_ms <= t_start:
            continue
        rows += 1
        status = int(row["status"])
        received_bytes += int(row["bytes"])
        if status == 200:
            successes += 1
            observations.append(float(row["total_ms"]))
        else:
            observations.append(float(meta["request_timeout_ms"]))

host_rows = []
with host_path.open(newline="") as handle:
    for row in csv.DictReader(handle):
        timestamp_ms = int(row["unix_s"]) * 1000
        if start_ms <= timestamp_ms < end_ms:
            host_rows.append(row)

service_rows = []
with service_path.open(newline="") as handle:
    for row in csv.DictReader(handle):
        timestamp_ms = int(row["unix_s"]) * 1000
        if start_ms <= timestamp_ms < end_ms:
            service_rows.append(row)

cpu_cores = [float(row["sim_cpu_pct"]) / 100.0 for row in host_rows]
rss_gib = [float(row["sim_rss_kb"]) / (1024.0 * 1024.0) for row in host_rows]
host_load1 = [float(row["load1"]) for row in host_rows]
mem_available_gib = [float(row["mem_available_kb"]) / (1024.0 * 1024.0) for row in host_rows]
swap_kib = [int(row["swap_used_kb"]) for row in host_rows]
tcp_established = [int(row["tcp_established"]) for row in host_rows]
sim_processes = [int(row["sim_processes"]) for row in host_rows]
expected_samples = max(1.0, duration_s / 5.0)
coverage = min(1.0, len(host_rows) / expected_samples)

log_text = log_path.read_text(errors="replace")
g5_pattern = re.compile(
    r"Unexpected error:|Rescue handler panic|client driver panic:|evaluation panic:|"
    r"http: panic serving|panic recovered|(^|\s)panic:|fatal error:|runtime: out of memory|"
    r"out of memory: killed process|service restart|restarting service|service unavailable|"
    r"service missing|unclean drain|did not drain within",
    re.IGNORECASE | re.MULTILINE,
)
g5_clean = g5_pattern.search(log_text) is None
enobufs_clean = "ENOBUFS" not in log_text and "no buffer space available" not in log_text.lower()

sample_runs = samples.get("runs", {})
if len(sample_runs) != 1:
    raise ValueError("sample audit must contain exactly one run")
sample = next(iter(sample_runs.values()))
success_rate = successes / rows if rows else 0.0
raw_score = quantile_type7(observations, 0.95) if observations else None
resource = {
    "host_samples": len(host_rows),
    "host_coverage_fraction": coverage,
    "sim_cpu_mean_cores": statistics.fmean(cpu_cores) if cpu_cores else None,
    "sim_cpu_p95_cores": quantile_type7(cpu_cores, 0.95) if cpu_cores else None,
    "sim_cpu_peak_cores": max(cpu_cores) if cpu_cores else None,
    "sim_rss_peak_gib": max(rss_gib) if rss_gib else None,
    "host_load1_mean": statistics.fmean(host_load1) if host_load1 else None,
    "host_load1_p95": quantile_type7(host_load1, 0.95) if host_load1 else None,
    "host_load1_peak": max(host_load1) if host_load1 else None,
    "mem_available_min_gib": min(mem_available_gib) if mem_available_gib else None,
    "swap_used_peak_kib": max(swap_kib) if swap_kib else None,
    "tcp_established_peak": max(tcp_established) if tcp_established else None,
    "sim_processes_min": min(sim_processes) if sim_processes else None,
    "sim_processes_max": max(sim_processes) if sim_processes else None,
    "postgres_processes_min": min((int(row["postgres_processes"]) for row in service_rows), default=0),
    "postgres_rss_peak_gib": max(
        (float(row["postgres_summed_rss_kb"]) / (1024.0 * 1024.0) for row in service_rows),
        default=0.0,
    ),
    "redis_processes_min": min((int(row["redis_processes"]) for row in service_rows), default=0),
    "redis_rss_peak_gib": max(
        (float(row["redis_summed_rss_kb"]) / (1024.0 * 1024.0) for row in service_rows),
        default=0.0,
    ),
}

expected_sim_processes = 1 + int(profile.get("fleet_shards", 4))

gates = {
    "completion": meta.get("completion_state") == "complete",
    "warm_pool": meta.get("clients_pool", 0) > 0 and meta.get("clients_established") == meta.get("clients_pool"),
    "success_rate_97pct": success_rate >= 0.97,
    "g5_clean": g5_clean,
    "enobufs_clean": enobufs_clean,
    "sample_count_nonzero": int(sample.get("samples", 0)) > 0,
    "sample_span_90pct": float(sample.get("sample_span_fraction", 0)) >= 0.90,
    "candidate_pools_nonempty": int(sample.get("empty_pools", -1)) == 0,
    "host_coverage_90pct": coverage >= 0.90,
    "sim_topology_exact": bool(sim_processes) and min(sim_processes) == expected_sim_processes and max(sim_processes) == expected_sim_processes,
    "cpu_mean_at_most_65pct": bool(cpu_cores) and statistics.fmean(cpu_cores) <= cpu_limit,
    "memory_headroom_20pct": bool(mem_available_gib) and min(mem_available_gib) >= mem_available_limit,
    "zero_swap": bool(swap_kib) and max(swap_kib) == 0,
    "postgres_present": resource["postgres_processes_min"] > 0,
    "redis_present": resource["redis_processes_min"] > 0,
}

result = {
    "schema": 1,
    "kind": "sim-latency-frontier-run-summary",
    "classification": "production_candidate_unprivileged",
    "profile": profile,
    "evaluation_id": meta.get("evaluation_id"),
    "identity": {
        "config_sha256": meta.get("config_sha256"),
        "build_revision": meta.get("build_revision"),
        "build_modified": meta.get("build_modified"),
        "num_cpu_recorded": meta.get("num_cpu"),
        "request_timeout_ms": meta.get("request_timeout_ms"),
        "measure_duration_s": duration_s,
        "run_manifest_sha256": hashlib.sha256(meta_bytes).hexdigest(),
        "results_csv_sha256": csv_hasher.hexdigest(),
    },
    "metrics": {
        "request_count": rows,
        "success_rate": success_rate,
        "received_bytes": received_bytes,
        "apex_raw_score_ms": raw_score,
        "ttfb_p95_ms": meta.get("metrics", {}).get("ttfb_p95_ms", {}).get("value"),
        "throughput_p50_bytes_per_s": meta.get("metrics", {}).get("throughput_p50_bytes_per_s", {}).get("value"),
        "goodput_bytes_per_s": meta.get("metrics", {}).get("goodput_bytes_per_s", {}).get("value"),
    },
    "samples": {
        "count": sample.get("samples"),
        "span_fraction": sample.get("sample_span_fraction"),
        "empty_pools": sample.get("empty_pools"),
        "load_p95_ms": sample.get("load_p95_ms"),
        "pool_count_mean": sample.get("report", {}).get("pool_count_mean"),
    },
    "resources": resource,
    "gates": gates,
    "frontier_eligible": all(gates.values()),
    "production_qualified": False,
    "production_blockers": [
        "PostgreSQL and Redis are outside the unprivileged 12-CPU job boundary",
        "SMT remains enabled system-wide",
        "CPU governor is not pinned to performance by this runner",
        "the simulator build is locally modified rather than a clean release",
    ],
}
json.dump(result, sys.stdout, indent=2, sort_keys=True, allow_nan=False)
sys.stdout.write("\n")
PY
    log "summary written: $output"
    jq '{frontier_eligible, metrics, samples, resources, failed_gates:[.gates|to_entries[]|select(.value == false)|.key]}' "$output"
}

run_profile() {
    local profile="${1:?missing profile name}"
    local tag="${2:?missing run tag}"
    local duration="${3:-5m}"
    local mode="${4:-impair}"
    require_name "$profile"
    require_name "$tag"
    [[ "$duration" =~ ^[1-9][0-9]*(s|m|h)$ ]] || die "duration must be an integer Go duration such as 5m"
    [ "$mode" = impair ] || [ "$mode" = no-impair ] || die "mode must be impair or no-impair"
    hardware_gate
    [ -x "$SIM_BIN" ] || die "frozen simulator binary is missing: $SIM_BIN"

    local pdir rdir profile_json config recorded_sha current_sha
    local recorded_binary_sha current_binary_sha
    local hosts fleet_shards
    pdir="$(profile_dir "$profile")"
    rdir="$(run_dir "$profile" "$tag")"
    profile_json="$pdir/profile.json"
    config="$pdir/providers.yml"
    [ -f "$profile_json" ] || die "profile is not initialized: $profile"
    [ -f "$config" ] || die "profile providers file is missing: $config"
    [ ! -e "$rdir" ] || die "run artifact path already exists: $rdir"
    recorded_sha="$(jq -er '.providers_sha256' "$profile_json")"
    current_sha="$(sha256_file "$config")"
    [ "$recorded_sha" = "$current_sha" ] || die "profile providers SHA-256 changed"
    recorded_binary_sha="$(jq -er '.simulator_sha256' "$profile_json")"
    current_binary_sha="$(sha256_file "$SIM_BIN")"
    [ "$recorded_binary_sha" = "$current_binary_sha" ] ||
        die "frozen simulator SHA-256 changed; initialize a new profile"
    hosts="$(jq -er '.hosts // 4' "$profile_json")"
    fleet_shards="$(jq -er '.fleet_shards // 4' "$profile_json")"
    if pgrep -x sim-latency >/dev/null 2>&1; then
        die "another sim-latency process is active"
    fi
    mkdir -p "$rdir"

    export WARP_HOST=127.0.0.1
    export WARP_BLOCK=sim
    export WARP_SERVICE=sim
    export WARP_VERSION=0.0.0-sim
    export WARP_ENV="${WARP_ENV:-local}"
    export WARP_DOMAIN="${WARP_DOMAIN:-bringyour.com}"
    export BRINGYOUR_POSTGRES_HOSTNAME="${BRINGYOUR_POSTGRES_HOSTNAME:-local-pg.bringyour.com}"
    export BRINGYOUR_REDIS_HOSTNAME="${BRINGYOUR_REDIS_HOSTNAME:-local-redis.bringyour.com}"
    ulimit -n 1048576 2>/dev/null || die "cannot raise nofile to 1048576"

    local host_sampler service_sampler run_pid rc auditor
    "$SCRIPT_DIR/sample-host-resources.sh" "$rdir/host.csv" "$HOST_SAMPLE_INTERVAL" &
    host_sampler=$!
    "$SCRIPT_DIR/sample-service-resources.sh" "$rdir/services.csv" "$HOST_SAMPLE_INTERVAL" &
    service_sampler=$!
    run_pid=""
    cleanup() {
        if [ -n "$run_pid" ]; then
            kill -TERM "$run_pid" 2>/dev/null || true
            wait "$run_pid" 2>/dev/null || true
        fi
        kill "$host_sampler" "$service_sampler" 2>/dev/null || true
        wait "$host_sampler" "$service_sampler" 2>/dev/null || true
    }
    trap cleanup EXIT INT TERM

    local args=(
        run --reset
        --providers "$config"
        --fleet-shards "$fleet_shards"
        --hosts "$hosts"
        --site-home "$ARTIFACT_ROOT/.sim-site"
        --duration "$duration"
        --evaluation-id "frontier-${profile}-${tag}"
        --meta "$rdir/run.json"
    )
    if [ "$mode" = no-impair ]; then args+=(--no-impair); fi
    log "run start profile=$profile tag=$tag duration=$duration mode=$mode cpuset=$CPUSET"
    set +e
    timeout --signal=TERM --kill-after=60 60m \
        taskset -c "$CPUSET" env GOMAXPROCS="$CPU_COUNT" \
        "$SIM_BIN" "${args[@]}" >"$rdir/results.csv" 2>"$rdir/stderr.log" &
    run_pid=$!
    wait "$run_pid"
    rc=$?
    run_pid=""
    set -e
    kill "$host_sampler" "$service_sampler" 2>/dev/null || true
    wait "$host_sampler" "$service_sampler" 2>/dev/null || true
    trap - EXIT INT TERM
    [ "$rc" -eq 0 ] || die "simulator exited $rc; artifacts retained at $rdir"
    [ -s "$rdir/run.json" ] || die "run manifest missing"
    [ -s "$rdir/run.json.complete.json" ] || die "completion marker missing"
    jq -e '.completion_state == "complete" and .clients_pool > 0 and .clients_established == .clients_pool' \
        "$rdir/run.json" >/dev/null || die "run did not complete with its full warm pool"
    if grep -Eiq "$G5_LOG_PATTERN" "$rdir/stderr.log"; then
        die "run emitted a G5 stability signature"
    fi

    auditor="$(ensure_sample_auditor)"
    "$auditor" "$rdir/run.json" >"$rdir/samples.json"
    summarize_run "$profile" "$tag"
}

usage() {
    printf '%s\n' \
        'usage: eval-frontier-12c.sh hardware' \
        '       eval-frontier-12c.sh init <profile> <providers> <clients> <rate/min> [seed] [quality-window] [hosts] [fleet-shards]' \
        '       eval-frontier-12c.sh run <profile> <run-tag> [duration] [impair|no-impair]' \
        '       eval-frontier-12c.sh summarize <profile> <run-tag>'
}

mkdir -p "$ARTIFACT_ROOT"
case "${1:-}" in
    hardware) hardware_json ;;
    init) shift; init_profile "$@" ;;
    run) shift; run_profile "$@" ;;
    summarize) shift; summarize_run "$@" ;;
    *) usage; exit 2 ;;
esac
