#!/usr/bin/env bash
#
# Finish an eval-48d local/directional baseline without silently accepting an
# underfilled campaign. This is intentionally post-campaign orchestration: it
# waits for an optional supervisor, tops up clean runs, authenticates the
# summary, then performs the documented held-out A/A workflow check.
#
# Usage:
#   WAIT_FOR_PID=<campaign-supervisor-pid> ./finalize-local-baseline.sh [min-runs] [topup-hours]

set -euo pipefail
cd "$(dirname "$0")"

min_runs="${1:-20}"
topup_hours="${2:-2}"
wait_for_pid="${WAIT_FOR_PID:-}"

case "$min_runs" in
    ''|*[!0-9]*) printf 'min-runs must be a positive integer\n' >&2; exit 2 ;;
esac
case "$topup_hours" in
    ''|*[!0-9]*) printf 'topup-hours must be a positive integer\n' >&2; exit 2 ;;
esac
[ "$min_runs" -gt 0 ] || { printf 'min-runs must be positive\n' >&2; exit 2; }
[ "$topup_hours" -gt 0 ] || { printf 'topup-hours must be positive\n' >&2; exit 2; }

log() { printf '[baseline-finalizer] %s %s\n' "$(date -u '+%F %T UTC')" "$*"; }

# Keep held-out runs under the same frozen stderr gate as campaign runs. A
# signature makes the run permanently unplaceable, so terminate before an
# authenticated marker can be admitted to the A/A comparison.
G5_LOG_PATTERN='Unexpected error:|Rescue handler panic|client driver panic:|evaluation panic:|http: panic serving|panic recovered|(^|[[:space:]])panic:|fatal error:|runtime: out of memory|out of memory: killed process|service restart|restarting service|service unavailable|service missing|unclean drain|did not drain within'

authenticated_inventory() {
    # A filename is not evidence of completion. Authenticate the inexpensive
    # marker -> manifest -> CSV chain before deciding that no top-up is needed;
    # summarize-baseline.py performs the deeper row/sample/telemetry audit.
    python3 -B - <<'PY'
import hashlib
import json
import re
from pathlib import Path


class InvalidArtifact(Exception):
    pass


def strict_object(pairs):
    value = {}
    for key, item in pairs:
        if key in value:
            raise InvalidArtifact(f"duplicate key: {key}")
        value[key] = item
    return value


def reject_constant(value):
    raise InvalidArtifact(f"non-standard JSON constant: {value}")


def read_json(path):
    payload = path.read_bytes()
    value = json.loads(
        payload.decode("utf-8"),
        object_pairs_hook=strict_object,
        parse_constant=reject_constant,
    )
    if not isinstance(value, dict):
        raise InvalidArtifact("top-level JSON is not an object")
    return value, payload


def sha256_file(path):
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for block in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


count = 0
numbers = []
root = Path("eval-48g/runs")
for marker_path in sorted(root.glob("r*.run.json.complete.json")):
    try:
        marker, marker_bytes = read_json(marker_path)
        manifest_path = Path(str(marker_path)[: -len(".complete.json")])
        manifest, manifest_bytes = read_json(manifest_path)
        csv_path = manifest_path.with_name(
            manifest_path.name[: -len(".run.json")] + ".csv"
        )
        if (
            marker.get("schema") != 1
            or marker.get("kind") != "sim-latency-complete"
            or marker.get("score_schema") != 1
            or marker.get("scorer_version") != "sim-latency-score/1"
            or manifest.get("schema") != 2
            or manifest.get("kind") != "sim-latency-run"
            or manifest.get("score_schema") != 1
            or manifest.get("scorer_version") != "sim-latency-score/1"
            or manifest.get("completion_state") != "complete"
            or not isinstance(manifest.get("evaluation_id"), str)
            or not manifest["evaluation_id"]
            or marker.get("evaluation_id") != manifest["evaluation_id"]
            or marker.get("completed_unix_ms") != manifest.get("completed_unix_ms")
            or marker.get("run_manifest_bytes") != len(manifest_bytes)
            or marker.get("run_manifest_sha256")
            != hashlib.sha256(manifest_bytes).hexdigest()
            or not csv_path.is_file()
            or manifest.get("results_csv_bytes") != csv_path.stat().st_size
            or manifest.get("results_csv_sha256") != sha256_file(csv_path)
        ):
            raise InvalidArtifact("artifact chain mismatch")
        match = re.fullmatch(r"r([0-9]+)\.run\.json\.complete\.json", marker_path.name)
        if match is None:
            raise InvalidArtifact("non-canonical attempt tag")
        count += 1
        numbers.append(int(match.group(1)))
    except (InvalidArtifact, OSError, UnicodeError, ValueError, TypeError):
        continue
longest = 0
current = 0
previous = None
for number in sorted(numbers):
    current = current + 1 if previous is not None and number == previous + 1 else 1
    longest = max(longest, current)
    previous = number
print(count, longest)
PY
}

heldout_stem_exists() {
    local stem="$1"
    [ -e "$stem.csv" ] ||
        [ -e "$stem.log" ] ||
        [ -e "$stem.run.json" ] ||
        [ -e "$stem.run.json.complete.json" ]
}

host_sampler_pid=""
service_sampler_pid=""
heldout_run_pid=""
heldout_guard_pid=""
start_resource_samplers() {
    ./sample-host-resources.sh eval-48g/campaign-host-resources.csv 30 &
    host_sampler_pid=$!
    ./sample-service-resources.sh eval-48g/campaign-service-resources.csv 30 &
    service_sampler_pid=$!
}
stop_resource_samplers() {
    if [ -n "$host_sampler_pid" ]; then
        kill "$host_sampler_pid" 2>/dev/null || true
        wait "$host_sampler_pid" 2>/dev/null || true
        host_sampler_pid=""
    fi
    if [ -n "$service_sampler_pid" ]; then
        kill "$service_sampler_pid" 2>/dev/null || true
        wait "$service_sampler_pid" 2>/dev/null || true
        service_sampler_pid=""
    fi
}
cleanup_finalizer() {
    if [ -n "$heldout_guard_pid" ]; then
        kill "$heldout_guard_pid" 2>/dev/null || true
        wait "$heldout_guard_pid" 2>/dev/null || true
        heldout_guard_pid=""
    fi
    if [ -n "$heldout_run_pid" ]; then
        kill -TERM "$heldout_run_pid" 2>/dev/null || true
        wait "$heldout_run_pid" 2>/dev/null || true
        heldout_run_pid=""
    fi
    stop_resource_samplers
}
trap cleanup_finalizer EXIT

run_guarded_heldout() {
    local tag="$1"
    local csv="$2"
    local meta="$3"
    local runlog="$4"
    local marker="$meta.complete.json"
    local g5_tainted=0
    local rc

    timeout --signal=TERM --kill-after=60 3300 \
        ./eval-48.sh run --meta "$meta" > "$csv" 2> "$runlog" &
    heldout_run_pid=$!
    (
        set +o pipefail
        tail --pid="$heldout_run_pid" -n +1 -F "$runlog" 2>/dev/null |
            grep -Eim1 "$G5_LOG_PATTERN" >/dev/null
    ) &
    heldout_guard_pid=$!

    while kill -0 "$heldout_run_pid" 2>/dev/null; do
        if ! kill -0 "$heldout_guard_pid" 2>/dev/null; then
            if wait "$heldout_guard_pid"; then
                g5_tainted=1
                log "held-out A/A run $tag emitted a frozen G5 signature; terminating it fail-closed"
                kill -TERM "$heldout_run_pid" 2>/dev/null || true
            fi
            heldout_guard_pid=""
            break
        fi
        sleep 1
    done

    if wait "$heldout_run_pid"; then
        rc=0
    else
        rc=$?
    fi
    heldout_run_pid=""
    if [ -n "$heldout_guard_pid" ]; then
        if wait "$heldout_guard_pid"; then
            g5_tainted=1
        fi
        heldout_guard_pid=""
    fi

    if [ "$g5_tainted" -eq 1 ] && [ -e "$marker" ]; then
        log "held-out ABORT: G5-tainted $tag wrote a completion marker; preserve artifacts for audit"
        return 2
    fi
    [ "$g5_tainted" -eq 0 ] && [ "$rc" -eq 0 ] && [ -s "$marker" ]
}

if [ -n "$wait_for_pid" ]; then
    log "waiting for campaign supervisor pid $wait_for_pid"
    while kill -0 "$wait_for_pid" 2>/dev/null; do
        sleep 30
    done
    # The campaign's host sampler polls the supervisor at the same cadence.
    # Let it observe the exit before a top-up starts another sampler.
    sleep 35
fi

mkdir -p eval-48g/runs eval-48g/heldout

while true; do
    read -r before before_streak < <(authenticated_inventory)
    if [ "$before" -ge "$min_runs" ] && [ "$before_streak" -ge "$min_runs" ]; then
        break
    fi
    log "authenticated runs $before (longest clean tag streak $before_streak/$min_runs); starting ${topup_hours}h top-up"
    start_resource_samplers
    if ./eval-48.sh campaign "$topup_hours"; then
        campaign_rc=0
    else
        campaign_rc=$?
    fi
    stop_resource_samplers
    read -r after after_streak < <(authenticated_inventory)
    log "top-up exit=$campaign_rc; authenticated runs $before -> $after; longest clean tag streak $before_streak -> $after_streak"
    if [ "$after" -le "$before" ]; then
        log "top-up made no progress; cooling down for 120s"
        sleep 120
    fi
done

read -r final_count final_streak < <(authenticated_inventory)
log "authenticated target reached ($final_count runs; longest clean tag streak $final_streak); rebuilding baseline"
./eval-48.sh baseline
log "authenticating and independently recomputing summary"
./eval-48.sh summary

heldout_csvs=()
while IFS= read -r marker; do
    csv="${marker%.run.json.complete.json}.csv"
    [ -s "$csv" ] && heldout_csvs+=("$csv")
done < <(find eval-48g/heldout -maxdepth 1 -type f \
    -name 'h*.run.json.complete.json' | sort)

next_index=1
while heldout_stem_exists "$(printf 'eval-48g/heldout/h%03d' "$next_index")"; do
    next_index=$((next_index + 1))
done

./sample-host-resources.sh eval-48g/heldout-host-resources.csv 30 &
host_sampler_pid=$!
./sample-service-resources.sh eval-48g/campaign-service-resources.csv 30 &
service_sampler_pid=$!
while [ "${#heldout_csvs[@]}" -lt 2 ]; do
    tag="$(printf 'h%03d' "$next_index")"
    csv="eval-48g/heldout/$tag.csv"
    meta="eval-48g/heldout/$tag.run.json"
    runlog="eval-48g/heldout/$tag.log"
    log "held-out A/A run $tag start"
    if run_guarded_heldout "$tag" "$csv" "$meta" "$runlog"; then
        heldout_csvs+=("$csv")
        log "held-out A/A run $tag authenticated"
    else
        heldout_rc=$?
        if [ "$heldout_rc" -eq 2 ]; then
            exit 1
        fi
        log "held-out A/A run $tag failed closed; preserving it as excluded"
    fi
    next_index=$((next_index + 1))
    if [ "${#heldout_csvs[@]}" -lt 2 ]; then
        log "held-out A/A cooldown 70s before the next attempt"
        sleep 70
    fi
done
stop_resource_samplers

aa_json="eval-48g/heldout-aa-compare.json"
./sim-latency compare \
    --a="${heldout_csvs[0]}" \
    --b="${heldout_csvs[1]}" \
    --baseline=eval-48g/baseline.json \
    --json > "$aa_json"
python3 - "$aa_json" <<'PY'
import json
import sys

path = sys.argv[1]
with open(path, encoding="utf-8") as handle:
    result = json.load(handle)
verdict = result.get("verdict")
if verdict != "indistinguishable":
    raise SystemExit(f"held-out A/A verdict was {verdict!r}, not 'indistinguishable'")
PY
log "held-out A/A verdict is indistinguishable; wrote $aa_json"
log "refreshing authenticated summary after held-out collection"
./eval-48.sh summary
log "running post-campaign artifact and test verification"
./verify-local-baseline.sh > eval-48g/postcampaign-verification.log 2>&1
log "post-campaign verification passed"
