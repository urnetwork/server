#!/usr/bin/env bash
#
# eval-48.sh -- the "eval-48" evaluation environment: the official sim-latency
# configuration, sized to fit a 48 GB machine with ample headroom. See
# README.md ("The eval-48 evaluation environment").
#
# The environment IS this script: the locked seed + scale constants below plus
# the standard run flags in `standard_run_args`. Changing any of them defines a
# different environment with a different noise floor -- re-measure the variance
# baseline before comparing runs across the change.
#
# Usage:
#   ./eval-48.sh init               generate + sha-verify the canonical providers file
#   ./eval-48.sh run [flags...]     one standard evaluation run (CSV on stdout;
#                                   meta defaults to eval-48g/run.json)
#   ./eval-48.sh campaign <hours>   sequential A/A replicates for <hours>, the
#                                   long-form variance baseline (artifacts under
#                                   eval-48g/runs/; failure-tolerant, resumable)
#   ./eval-48.sh baseline           compute eval-48g/baseline.json from every
#                                   completed campaign run
#
# Requires server/local/run-local.sh (postgres + redis) to be up. For an
# unattended campaign wrap it in caffeinate and keep the lid open:
#   caffeinate -i ./eval-48.sh campaign 24

set -u -o pipefail
cd "$(dirname "$0")"

# ---- the locked environment (revision eval-48b, 2026-07-31) -----------------
# eval-48b reshapes the throughput distribution so thr_p95 is a decidable
# signal: mixture v2 (business tier + narrowed hosting + tighter caps),
# two-tier site bodies (2-6 MiB download tier, throughput gate 1 MiB), and
# the arrival rate rebalanced to keep the same offered-bytes regime. The
# eval-48a revision (rate 200, sha eca46fe8...) and its k=12 baseline are
# archived under eval-48g/*-eval48a.
SEED=48
COUNT=2000        # providers
CLIENTS=200       # warm client identity pool
RATE=80           # mean client arrivals per minute (crawl weight ~2.4x eval-48a)
DURATION=30m      # measured window
FLEET_SHARDS=4    # provider fleet subprocesses
# canonical fleet file: regenerated bit-identically from the seed; the sha
# pins the exact workload (compare refuses runs of a different sha)
PROVIDERS=eval-48g/providers-eval48b.yml
PROVIDERS_SHA=7851a0d0c0d2c80c4c28f0ecd752305f11a87087cf16d7056ad5ea8f027dfd26
# everything else is the sim-latency default: ramp 1m, prewarm 13h, settle 1m,
# hosts 4, pipeline-interval 10s, test-timeout 3s, announce-timeout 2s
# ----------------------------------------------------------------------------

export WARP_HOST="${WARP_HOST:-127.0.0.1}"
export WARP_BLOCK="${WARP_BLOCK:-sim}"
export WARP_SERVICE="${WARP_SERVICE:-sim}"
export WARP_VERSION="${WARP_VERSION:-0.0.0-sim}"
export WARP_ENV="${WARP_ENV:-local}"
export BRINGYOUR_POSTGRES_HOSTNAME="${BRINGYOUR_POSTGRES_HOSTNAME:-local-pg.bringyour.com}"
export BRINGYOUR_REDIS_HOSTNAME="${BRINGYOUR_REDIS_HOSTNAME:-local-redis.bringyour.com}"
ulimit -n 1048576 2>/dev/null || true

log() { printf '[eval-48] %s %s\n' "$(date '+%F %T')" "$*"; }
die() { log "$*"; exit 1; }

require_binary() {
    [ -x ./sim-latency ] || die "./sim-latency binary missing; build with: go build -o sim-latency ."
}

# init: generate the canonical providers file from the seed if missing, then
# verify the sha so every machine runs the identical workload.
eval48_init() {
    require_binary
    mkdir -p eval-48g
    if [ ! -s "$PROVIDERS" ]; then
        log "generating $PROVIDERS (seed $SEED, $COUNT providers)"
        ./sim-latency init --count "$COUNT" --clients "$CLIENTS" --rate "$RATE" \
            --seed "$SEED" --out "$PROVIDERS" || die "init failed"
    fi
    sha=$(shasum -a 256 "$PROVIDERS" | awk '{print $1}')
    [ "$sha" = "$PROVIDERS_SHA" ] || die "$PROVIDERS sha $sha != canonical $PROVIDERS_SHA
regenerate it: rm $PROVIDERS && ./eval-48.sh init (or update PROVIDERS_SHA if the environment intentionally changed)"
    log "$PROVIDERS verified ($sha)"
}

standard_run_args() {
    echo "run --reset --providers $PROVIDERS --fleet-shards $FLEET_SHARDS --site-home eval-48g/.sim-site --duration $DURATION"
}

eval48_run() {
    eval48_init
    extra=("$@")
    case " ${extra[*]-} " in
        *" --meta"*) ;;
        *) extra+=("--meta" "eval-48g/run.json") ;;
    esac
    # shellcheck disable=SC2046
    exec ./sim-latency $(standard_run_args) "${extra[@]}"
}

# campaign: sequential independent `run --reset` replicates until the deadline.
# The same measurement `sim-latency baseline --replicates` makes, but
# failure-tolerant and time-bounded for long unattended campaigns: one failed
# replicate is logged and skipped; three consecutive failures abort (the
# environment itself is down, e.g. the local stack stopped).
eval48_campaign() {
    hours="${1:?usage: eval-48.sh campaign <hours>}"
    eval48_init
    if pgrep -f "sim-latency run" > /dev/null 2>&1; then
        die "a sim-latency run is already active; refusing to start a campaign"
    fi
    mkdir -p eval-48g/runs

    ./sample-rss.sh eval-48g/campaign-rss.csv 30 &
    sampler_pid=$!
    trap 'kill "$sampler_pid" 2>/dev/null' EXIT

    # clear any stale processes from an interrupted earlier campaign
    pkill -f "sim-latency (run|fleet)" 2>/dev/null && sleep 5

    deadline=$(( $(date +%s) + hours * 3600 ))
    i=1
    while [ -e "$(printf 'eval-48g/runs/r%03d.csv' "$i")" ]; do i=$((i + 1)); done
    log "campaign start: ${hours}h, first run r$(printf '%03d' "$i")"

    # per-run watchdog: a starved warm-up can wedge a replicate indefinitely
    # (observed 2026-07-30: 2h9m stuck after prewarm under an external CPU
    # storm). A wedged run should cost one FAILED replicate, not hours of
    # campaign budget. 3300s = 55m >> the ~36m standard run.
    watchdog=""
    if command -v timeout > /dev/null 2>&1; then
        watchdog="timeout --signal=TERM --kill-after=60 3300"
    fi

    consecutive_failures=0
    completed=0
    while [ "$(date +%s)" -lt "$deadline" ]; do
        tag=$(printf 'r%03d' "$i")
        csv="eval-48g/runs/$tag.csv"
        meta="eval-48g/runs/$tag.run.json"
        runlog="eval-48g/runs/$tag.log"
        log "run $tag start"
        t0=$(date +%s)
        # shellcheck disable=SC2046,SC2086
        $watchdog ./sim-latency $(standard_run_args) --meta "$meta" > "$csv" 2> "$runlog"
        rc=$?
        wall=$(( $(date +%s) - t0 ))
        if [ "$rc" -eq 0 ] && [ -s "$meta" ]; then
            consecutive_failures=0
            completed=$((completed + 1))
            summary=$(python3 - "$meta" <<'PY'
import json, sys
try:
    d = json.load(open(sys.argv[1]))
    m = d.get("metrics", {})
    def v(k, fmt):
        x = m.get(k, {}).get("value")
        # a run that never opened its window has no metrics; show "-"
        return fmt.format(x) if x is not None else "-"
    print(
        f"estab={d.get('clients_established')}/{d.get('clients_pool')}"
        f" rows={d.get('rows_in_window')}"
        f" fail={v('fail_rate', '{:.4f}')}"
        f" ttfb_p95={v('ttfb_p95_ms', '{:.0f}')}ms"
        f" thr_p95={v('throughput_p95_bytes_per_s', '{:.0f}')}B/s"
    )
except Exception as e:
    print(f"meta-parse-error: {e}")
PY
)
            log "run $tag done exit=0 wall=${wall}s $summary"
        else
            consecutive_failures=$((consecutive_failures + 1))
            log "run $tag FAILED exit=$rc wall=${wall}s (consecutive $consecutive_failures/3; see $runlog)"
            pkill -f "sim-latency fleet" 2>/dev/null
            if [ "$consecutive_failures" -ge 3 ]; then
                log "campaign ABORT: 3 consecutive run failures; is server/local/run-local.sh still up?"
                exit 1
            fi
            sleep 60
        fi
        i=$((i + 1))
        sleep 10
    done
    log "campaign complete: $completed runs completed (through $(printf 'r%03d' $((i - 1))))"
}

# baseline: the noise floor + convergence from every completed campaign run.
# A run is complete when its run.json side-car exists (written at run end), so
# an in-flight replicate's partial csv is never included.
eval48_baseline() {
    require_binary
    runs=""
    for csv in eval-48g/runs/r*.csv; do
        [ -s "${csv%.csv}.run.json" ] || continue
        runs="${runs:+$runs,}$csv"
    done
    [ -n "$runs" ] || die "no completed campaign runs under eval-48g/runs/"
    ./sim-latency baseline --runs "$runs" --out eval-48g/baseline.json
}

case "${1:-}" in
    init)     eval48_init ;;
    run)      shift; eval48_run "$@" ;;
    campaign) shift; eval48_campaign "$@" ;;
    baseline) eval48_baseline ;;
    *)        sed -n '2,/^$/p' "$0" | sed 's/^#\{0,1\} \{0,1\}//'; exit 1 ;;
esac
