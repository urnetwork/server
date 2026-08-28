#!/usr/bin/env bash
# Immutable worker-side runner for Apex ur_latency score_schema 1.
#
# This script intentionally has no uncalibrated defaults. The evaluation
# service publishes and supplies every APEX_* value after qualification; a
# missing value fails closed. See OFFICIAL-RUN.md.

set -Eeuo pipefail
umask 077

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
readonly SERVER_ROOT="${APEX_SERVER_ROOT:-$(cd "$SCRIPT_DIR/../.." && pwd -P)}"

log() { printf '[official-run] %s\n' "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }
require_command() { command -v "$1" >/dev/null 2>&1 || die "required command missing: $1"; }
require_var() {
    local name="$1"
    [ -n "${!name:-}" ] || die "required environment variable missing: $name"
}

sha256_file() { sha256sum "$1" | awk '{print $1}'; }
require_sha256() {
    local value="$1"
    [[ "$value" =~ ^[0-9a-f]{64}$ ]] || die "invalid SHA-256: $value"
}
require_git_sha() {
    local value="$1"
    [[ "$value" =~ ^[0-9a-f]{40}$ ]] || die "invalid git SHA: $value"
}
require_evaluation_id() {
    [[ "$APEX_EVALUATION_ID" =~ ^[A-Za-z0-9._-]{1,128}$ ]] ||
        die "APEX_EVALUATION_ID must match [A-Za-z0-9._-]{1,128}"
}

require_common() {
    local names=(
        APEX_BASE_SHA APEX_BUILD_SHA APEX_SCORER_BIN APEX_SCORER_SHA256
        APEX_SIM_BIN APEX_SIM_SHA256 APEX_PROVIDERS_FILE APEX_PROVIDERS_SHA256
        APEX_ARTIFACT_ROOT APEX_EVALUATION_ID APEX_API_IMAGE_DIGEST
        APEX_HARDWARE_ID APEX_HOST_QUALIFICATION_SHA256
        APEX_KERNEL_RELEASE APEX_MICROCODE_REVISION
        APEX_PATCH_FILE APEX_PATCH_SHA256
        APEX_CPU_COUNT
        APEX_DURATION APEX_REQUEST_TIMEOUT APEX_RAMP APEX_PREWARM
        APEX_SETTLE APEX_CLIENT_WARMUP_TIMEOUT APEX_FLEET_SHARDS APEX_HOSTS APEX_PIPELINE_INTERVAL
        APEX_TEST_TIMEOUT APEX_ANNOUNCE_TIMEOUT APEX_SITE_LISTEN
        APEX_API_PORT APEX_NO_IMPAIR APEX_WALL_TIMEOUT APEX_KILL_AFTER
    )
    local name
    for name in "${names[@]}"; do require_var "$name"; done

    require_git_sha "$APEX_BASE_SHA"
    require_git_sha "$APEX_BUILD_SHA"
    require_sha256 "$APEX_SCORER_SHA256"
    require_sha256 "$APEX_SIM_SHA256"
    require_sha256 "$APEX_PROVIDERS_SHA256"
    require_sha256 "$APEX_PATCH_SHA256"
    require_sha256 "$APEX_HOST_QUALIFICATION_SHA256"
    require_evaluation_id
    [[ "$APEX_API_IMAGE_DIGEST" =~ ^sha256:[0-9a-f]{64}$ ]] ||
        die "APEX_API_IMAGE_DIGEST must be a sha256 digest"
    [[ "$APEX_ARTIFACT_ROOT" = /* && "$APEX_ARTIFACT_ROOT" != / ]] ||
        die "APEX_ARTIFACT_ROOT must be an absolute non-root path"
    [ -x "$APEX_SCORER_BIN" ] || die "scorer binary is not executable"
    [ -x "$APEX_SIM_BIN" ] || die "simulator binary is not executable"
    [ -f "$APEX_PROVIDERS_FILE" ] || die "providers file is missing"
    [ -f "$APEX_PATCH_FILE" ] || die "canonical patch file is missing"
    [ "$(sha256_file "$APEX_PATCH_FILE")" = "$APEX_PATCH_SHA256" ] ||
        die "canonical patch hash mismatch"
    [ "$APEX_NO_IMPAIR" = yes ] || [ "$APEX_NO_IMPAIR" = no ] ||
        die "APEX_NO_IMPAIR must be yes or no"
    [[ "$APEX_CPU_COUNT" =~ ^[1-9][0-9]*$ ]] ||
        die "APEX_CPU_COUNT must be a positive integer"
}

verify_binary_revision() {
    local binary="$1" expected="$2" label="$3"
    local revision modified
    revision="$(go version -m "$binary" | sed -n 's/.*vcs\.revision=//p')"
    modified="$(go version -m "$binary" | sed -n 's/.*vcs\.modified=//p')"
    [ "$revision" = "$expected" ] ||
        die "$label revision $revision does not match $expected"
    [ "$modified" = false ] || die "$label was built from a modified worktree"
}

preflight() {
    require_common
    require_command go
    require_command git
    require_command jq
    require_command sha256sum
    require_command stat
    require_command sync
    require_command timeout

    [ "$(sha256_file "$APEX_SCORER_BIN")" = "$APEX_SCORER_SHA256" ] ||
        die "scorer binary hash mismatch"
    [ "$(sha256_file "$APEX_SIM_BIN")" = "$APEX_SIM_SHA256" ] ||
        die "simulator binary hash mismatch"
    [ "$(sha256_file "$APEX_PROVIDERS_FILE")" = "$APEX_PROVIDERS_SHA256" ] ||
        die "providers file hash mismatch"
    verify_binary_revision "$APEX_SIM_BIN" "$APEX_BUILD_SHA" simulator

    [ "$(git -C "$SERVER_ROOT" rev-parse HEAD)" = "$APEX_BUILD_SHA" ] ||
        die "server checkout is not at APEX_BUILD_SHA"
    git -C "$SERVER_ROOT" diff --quiet --ignore-submodules -- ||
        die "server checkout has tracked worktree modifications"
    git -C "$SERVER_ROOT" diff --cached --quiet --ignore-submodules -- ||
        die "server checkout has staged modifications"
    [ -z "$(git -C "$SERVER_ROOT" status --porcelain --untracked-files=all)" ] ||
        die "server checkout has untracked worktree content"
    git -C "$SERVER_ROOT" merge-base --is-ancestor "$APEX_BASE_SHA" "$APEX_BUILD_SHA" ||
        die "candidate build does not descend from pinned base"

    [ "$(uname -r)" = "$APEX_KERNEL_RELEASE" ] ||
        die "kernel $(uname -r) does not match pinned $APEX_KERNEL_RELEASE"
    local microcode
    microcode="$(awk -F: '$1 ~ /^microcode/ {gsub(/[[:space:]]/, "", $2); print $2}' /proc/cpuinfo | sort -u | paste -sd, -)"
    [ -n "$microcode" ] && [ "$microcode" = "$APEX_MICROCODE_REVISION" ] ||
        die "microcode $microcode does not match pinned $APEX_MICROCODE_REVISION"
    [ "$(nproc)" -eq "$APEX_CPU_COUNT" ] ||
        die "evaluation container must expose exactly $APEX_CPU_COUNT CPUs"
    local memory_kib
    memory_kib="$(awk '/^MemTotal:/ {print $2}' /proc/meminfo)"
    [ "${memory_kib:-0}" -ge 120000000 ] || die "official host must have at least 120,000,000 KiB RAM"
    local nofile
    nofile="$(ulimit -n)"
    if [ "$nofile" != unlimited ]; then
        [ "$nofile" -ge 1048576 ] || die "file descriptor hardening is not active"
    fi
    # shellcheck disable=SC1091
    . /etc/os-release
    [ "${ID:-}" = ubuntu ] && [ "${VERSION_ID:-}" = 24.04 ] ||
        die "official host must run Ubuntu 24.04"

    local cgroup_path
    cgroup_path="$(awk -F: '$1 == "0" {print $3}' /proc/self/cgroup)"
    [ -n "$cgroup_path" ] || die "cgroup v2 membership is unavailable"
    if [ "$cgroup_path" = / ]; then
        # A private cgroup namespace presents the container's delegated cgroup
        # as its own root. Do not confuse that view with the unbounded host
        # root: both memory and PID controllers must be present and finite.
        [ -r /sys/fs/cgroup/cgroup.controllers ] ||
            die "private cgroup namespace has no cgroup v2 controllers"
        [ -r /sys/fs/cgroup/memory.max ] && [ -r /sys/fs/cgroup/pids.max ] ||
            die "private cgroup namespace is missing required controller limits"
        local memory_max pids_max
        memory_max="$(</sys/fs/cgroup/memory.max)"
        pids_max="$(</sys/fs/cgroup/pids.max)"
        [[ "$memory_max" =~ ^[1-9][0-9]*$ ]] ||
            die "private cgroup namespace has no finite memory limit"
        [[ "$pids_max" =~ ^[1-9][0-9]*$ ]] ||
            die "private cgroup namespace has no finite PID limit"
    fi

    [ "${APEX_CALIBRATION_ACCEPTED:-}" = yes ] ||
        die "APEX_CALIBRATION_ACCEPTED=yes is required; no unqualified scale may run officially"

    log "preflight passed: eval=$APEX_EVALUATION_ID build=$APEX_BUILD_SHA hardware=$APEX_HARDWARE_ID"
}

job_dir() {
    printf '%s/%s' "$APEX_ARTIFACT_ROOT" "$APEX_EVALUATION_ID"
}

run_one() {
    preflight
    local dir
    dir="$(job_dir)"
    [ ! -e "$dir" ] || die "job artifact path already exists: $dir"
    mkdir -m 0700 -p "$dir"
    mkdir -m 0700 "$dir/site"

    local csv="$dir/results.csv"
    local stderr="$dir/stderr.log"
    local meta="$dir/run.json"
    local marker="$dir/run.complete.json"
    local accounting="$dir/accounting.json"
    local accounting_source="$dir/accounting.source.json"
    local resources="$dir/resources.json"

    local args=(
        run --official --reset
        --expected-revision "$APEX_BUILD_SHA"
        --evaluation-id "$APEX_EVALUATION_ID"
        --providers "$APEX_PROVIDERS_FILE"
        --site-home "$dir/site"
        --meta "$meta"
        --final-marker "$marker"
        --accounting-report "$accounting"
        --accounting-source "$accounting_source"
        --resource-report "$resources"
        --duration "$APEX_DURATION"
        --request-timeout "$APEX_REQUEST_TIMEOUT"
        --ramp "$APEX_RAMP"
        --prewarm "$APEX_PREWARM"
        --settle "$APEX_SETTLE"
        --client-warmup-timeout "$APEX_CLIENT_WARMUP_TIMEOUT"
        --fleet-shards "$APEX_FLEET_SHARDS"
        --site-listen "$APEX_SITE_LISTEN"
        --hosts "$APEX_HOSTS"
        --api-port "$APEX_API_PORT"
        --pipeline-interval "$APEX_PIPELINE_INTERVAL"
        --test-timeout "$APEX_TEST_TIMEOUT"
        --announce-timeout "$APEX_ANNOUNCE_TIMEOUT"
    )
    if [ "$APEX_NO_IMPAIR" = yes ]; then args+=(--no-impair); fi

    log "starting simulator; artifacts=$dir"
    set +e
    timeout --signal=TERM --kill-after="$APEX_KILL_AFTER" "$APEX_WALL_TIMEOUT" \
        "$APEX_SIM_BIN" "${args[@]}" >"$csv" 2>"$stderr"
    local rc=$?
    set -e
    [ "$rc" -eq 0 ] || die "simulator failed with exit $rc; incomplete artifacts retained at $dir"
    [ -s "$csv" ] || die "simulator emitted no CSV"
    [ -s "$meta" ] || die "simulator emitted no run manifest"
    [ -s "$marker" ] || die "simulator emitted no completion marker"
    [ -s "$accounting_source" ] || die "simulator emitted no provider accounting source"
    jq -e '.schema == 2 and .score_schema == 1 and .completion_state == "complete"' "$meta" >/dev/null ||
        die "run manifest is not complete schema 2"
    jq -e --arg evaluation_id "$APEX_EVALUATION_ID" \
        '.schema == 1 and
         .kind == "sim-latency-provider-accounting-source" and
         .evaluation_id == $evaluation_id and
         .complete == true and
         .provider_egress_bytes >= 0' \
        "$accounting_source" >/dev/null || die "provider accounting source is incomplete"
    jq -e \
        --arg source_path "$accounting_source" \
        --arg source_sha256 "$(sha256_file "$accounting_source")" \
        --argjson source_bytes "$(stat -c '%s' "$accounting_source")" \
        '.accounting_source_path == $source_path and
         .accounting_source_sha256 == $source_sha256 and
         .accounting_source_bytes == $source_bytes' \
        "$meta" >/dev/null || die "run manifest does not authenticate provider accounting source"

    log "simulator complete; the worker must now write immutable accounting.json and resources.json"
    printf '%s\n' "$dir"
}

build_baseline_bundle() {
    require_common
    require_var APEX_ROUND_ID
    require_var APEX_TAKEOVER_MARGIN
    require_var APEX_BASELINE_RUNS
    require_var APEX_BASELINE_STDERR
    require_var APEX_BASELINE_ACCOUNTING
    require_var APEX_BASELINE_SAMPLES
    require_var APEX_BASELINE_RESOURCES
    require_var APEX_BASELINE_MARKERS
    require_var APEX_BASELINE_MANIFEST
    [[ "$APEX_ROUND_ID" =~ ^[A-Za-z0-9._-]{1,128}$ ]] ||
        die "APEX_ROUND_ID must match [A-Za-z0-9._-]{1,128}"
    [[ "$APEX_TAKEOVER_MARGIN" =~ ^0\.[0-9]+$ ]] ||
        die "APEX_TAKEOVER_MARGIN must be a decimal fraction"
    [ ! -e "$APEX_BASELINE_MANIFEST" ] || die "baseline manifest path already exists"
    [ "$(sha256_file "$APEX_SCORER_BIN")" = "$APEX_SCORER_SHA256" ] ||
        die "scorer binary hash mismatch"

    "$APEX_SCORER_BIN" score-baseline \
        --run "$APEX_BASELINE_RUNS" \
        --stderr "$APEX_BASELINE_STDERR" \
        --accounting "$APEX_BASELINE_ACCOUNTING" \
        --samples "$APEX_BASELINE_SAMPLES" \
        --resource-report "$APEX_BASELINE_RESOURCES" \
        --marker "$APEX_BASELINE_MARKERS" \
        --round-id "$APEX_ROUND_ID" \
        --takeover-margin "$APEX_TAKEOVER_MARGIN" \
        --out "$APEX_BASELINE_MANIFEST" >/dev/null
    jq -e '.score_schema == 1 and .kind == "sim-latency-score-baseline" and
        (.replicates | length % 2 == 1) and
        ([.replicates[].findproviders_sample_span_fraction |
          type == "number" and isfinite and . >= 0.90 and . <= 1] | all)' \
        "$APEX_BASELINE_MANIFEST" >/dev/null || die "baseline builder emitted an invalid manifest"
    log "baseline manifest written: $APEX_BASELINE_MANIFEST sha256=$(sha256_file "$APEX_BASELINE_MANIFEST")"
}

score_bundle() {
    require_common
    require_var APEX_BASELINE_MANIFEST
    require_var APEX_BASELINE_SHA256
    require_var APEX_CANDIDATE_RUNS
    require_var APEX_CANDIDATE_STDERR
    require_var APEX_CANDIDATE_ACCOUNTING
    require_var APEX_CANDIDATE_SAMPLES
    require_var APEX_CANDIDATE_RESOURCES
    require_var APEX_CANDIDATE_MARKERS
    require_var APEX_SCORE_OUTPUT
    [ -f "$APEX_BASELINE_MANIFEST" ] || die "baseline manifest missing"
    require_sha256 "$APEX_BASELINE_SHA256"
    [ "$(sha256_file "$APEX_BASELINE_MANIFEST")" = "$APEX_BASELINE_SHA256" ] ||
        die "baseline manifest hash mismatch"
    [ ! -e "$APEX_SCORE_OUTPUT" ] || die "score output path already exists"

    [ "$(sha256_file "$APEX_SCORER_BIN")" = "$APEX_SCORER_SHA256" ] ||
        die "scorer binary hash mismatch"
    "$APEX_SCORER_BIN" score \
        --run "$APEX_CANDIDATE_RUNS" \
        --stderr "$APEX_CANDIDATE_STDERR" \
        --baseline "$APEX_BASELINE_MANIFEST" \
        --accounting "$APEX_CANDIDATE_ACCOUNTING" \
        --samples "$APEX_CANDIDATE_SAMPLES" \
        --resource-report "$APEX_CANDIDATE_RESOURCES" \
        --marker "$APEX_CANDIDATE_MARKERS" \
        --out "$APEX_SCORE_OUTPUT" >/dev/null
    jq -e '.score_schema == 1 and (.placeable | type == "boolean")' "$APEX_SCORE_OUTPUT" >/dev/null ||
        die "scorer did not emit score_schema 1"
    log "score written to $APEX_SCORE_OUTPUT"
}

hash_tree() {
    local path="$1"
    if [ -f "$path" ]; then
        sha256_file "$path"
    elif [ -d "$path" ]; then
        (
            cd "$path"
            find . -type f -print0 | LC_ALL=C sort -z | xargs -0 -r sha256sum | sha256sum | awk '{print $1}'
        )
    else
        die "artifact missing: $path"
    fi
}

finalize_bundle() {
    preflight
    require_var APEX_SCORE_OUTPUT
    require_var APEX_BASELINE_MANIFEST
    require_var APEX_BASELINE_SHA256
    require_sha256 "$APEX_BASELINE_SHA256"
    [ "$(sha256_file "$APEX_BASELINE_MANIFEST")" = "$APEX_BASELINE_SHA256" ] ||
        die "baseline manifest hash mismatch"
    local dir manifest complete
    dir="$(job_dir)"
    manifest="$dir/evaluation-manifest.json"
    complete="$dir/evaluation.complete.json"
    [ -d "$dir" ] || die "job artifact directory missing"
    [ ! -e "$manifest" ] || die "evaluation manifest already exists"
    [ ! -e "$complete" ] || die "final evaluation marker already exists"
    local required=(
        "$APEX_PATCH_FILE" "$APEX_PROVIDERS_FILE" "$dir/results.csv"
        "$dir/stderr.log" "$dir/run.json" "$dir/run.complete.json"
        "$dir/accounting.source.json" "$dir/accounting.json"
        "$dir/resources.json" "$APEX_BASELINE_MANIFEST"
        "$APEX_SCORE_OUTPUT"
    )
    local artifact
    for artifact in "${required[@]}"; do
        [ -f "$artifact" ] || die "required artifact missing: $artifact"
        if [ "$artifact" != "$APEX_PATCH_FILE" ]; then
            [ -s "$artifact" ] || die "required artifact empty: $artifact"
        fi
    done
    sync -d "${required[@]}"
    local stats_root
    stats_root="$(jq -er '.stats_root' "$dir/run.json")" || die "run manifest has no stats_root"
    case "$stats_root" in
        "$dir/site/stats/"*) ;;
        *) die "run stats_root escapes the per-job site directory" ;;
    esac
    [ -d "$stats_root" ] || die "run stats_root is missing"
    [ -z "$(find "$stats_root" -type f -name '*.partial' -print -quit)" ] ||
        die "run stats_root contains an unfinished segment"

    jq -n \
        --arg evaluation_id "$APEX_EVALUATION_ID" \
        --arg base_sha "$APEX_BASE_SHA" \
        --arg build_sha "$APEX_BUILD_SHA" \
        --arg patch_sha256 "$APEX_PATCH_SHA256" \
        --arg api_image_digest "$APEX_API_IMAGE_DIGEST" \
        --arg providers_sha256 "$APEX_PROVIDERS_SHA256" \
        --arg scorer_sha256 "$APEX_SCORER_SHA256" \
        --arg simulator_sha256 "$APEX_SIM_SHA256" \
        --arg hardware_id "$APEX_HARDWARE_ID" \
        --arg host_qualification_sha256 "$APEX_HOST_QUALIFICATION_SHA256" \
        --arg kernel_release "$APEX_KERNEL_RELEASE" \
        --arg microcode_revision "$APEX_MICROCODE_REVISION" \
        --arg csv_sha256 "$(sha256_file "$dir/results.csv")" \
        --arg stderr_sha256 "$(sha256_file "$dir/stderr.log")" \
        --arg run_sha256 "$(sha256_file "$dir/run.json")" \
        --arg run_marker_sha256 "$(sha256_file "$dir/run.complete.json")" \
        --arg accounting_source_sha256 "$(sha256_file "$dir/accounting.source.json")" \
        --arg accounting_sha256 "$(sha256_file "$dir/accounting.json")" \
        --arg resources_sha256 "$(sha256_file "$dir/resources.json")" \
        --arg baseline_sha256 "$APEX_BASELINE_SHA256" \
        --arg samples_sha256 "$(hash_tree "$stats_root")" \
        --arg score_sha256 "$(sha256_file "$APEX_SCORE_OUTPUT")" \
        --arg duration "$APEX_DURATION" \
        --arg request_timeout "$APEX_REQUEST_TIMEOUT" \
        --arg ramp "$APEX_RAMP" \
        --arg prewarm "$APEX_PREWARM" \
        --arg settle "$APEX_SETTLE" \
        --arg client_warmup_timeout "$APEX_CLIENT_WARMUP_TIMEOUT" \
        --arg fleet_shards "$APEX_FLEET_SHARDS" \
        --arg hosts "$APEX_HOSTS" \
        --arg site_listen "$APEX_SITE_LISTEN" \
        --arg api_port "$APEX_API_PORT" \
        --arg pipeline_interval "$APEX_PIPELINE_INTERVAL" \
        --arg test_timeout "$APEX_TEST_TIMEOUT" \
        --arg announce_timeout "$APEX_ANNOUNCE_TIMEOUT" \
        --arg no_impair "$APEX_NO_IMPAIR" \
        --arg wall_timeout "$APEX_WALL_TIMEOUT" \
        --arg kill_after "$APEX_KILL_AFTER" \
        '{
            schema: 1,
            kind: "sim-latency-evaluation-manifest",
            score_schema: 1,
            evaluation_id: $evaluation_id,
            base_sha: $base_sha,
            build_sha: $build_sha,
            patch_sha256: $patch_sha256,
            api_image_digest: $api_image_digest,
            providers_sha256: $providers_sha256,
            scorer_sha256: $scorer_sha256,
            simulator_sha256: $simulator_sha256,
            hardware_id: $hardware_id,
            host_qualification_sha256: $host_qualification_sha256,
            kernel_release: $kernel_release,
            microcode_revision: $microcode_revision,
            flags: {
                duration: $duration,
                request_timeout: $request_timeout,
                ramp: $ramp,
                prewarm: $prewarm,
                settle: $settle,
                client_warmup_timeout: $client_warmup_timeout,
                fleet_shards: $fleet_shards,
                hosts: $hosts,
                site_listen: $site_listen,
                api_port: $api_port,
                pipeline_interval: $pipeline_interval,
                test_timeout: $test_timeout,
                announce_timeout: $announce_timeout,
                no_impair: $no_impair,
                wall_timeout: $wall_timeout,
                kill_after: $kill_after
            },
            artifacts: {
                csv: $csv_sha256,
                stderr: $stderr_sha256,
                run: $run_sha256,
                run_marker: $run_marker_sha256,
                accounting_source: $accounting_source_sha256,
                accounting: $accounting_sha256,
                resources: $resources_sha256,
                baseline: $baseline_sha256,
                samples: $samples_sha256,
                score: $score_sha256
            }
        }' >"$manifest"
    chmod 0600 "$manifest"
    sync -d "$manifest"
    sync "$dir"

    jq -n \
        --arg evaluation_id "$APEX_EVALUATION_ID" \
        --arg manifest_sha256 "$(sha256_file "$manifest")" \
        '{schema: 1, kind: "sim-latency-evaluation-complete", score_schema: 1,
          evaluation_id: $evaluation_id, evaluation_manifest_sha256: $manifest_sha256}' >"$complete"
    chmod 0600 "$complete"
    sync -d "$complete"
    sync "$dir"
    log "immutable evaluation bundle complete: $complete"
}

usage() {
    printf '%s\n' \
        'usage: official-run.sh preflight' \
        '       official-run.sh run' \
        '       official-run.sh baseline' \
        '       official-run.sh score' \
        '       official-run.sh finalize'
}

case "${1:-}" in
    preflight) preflight ;;
    run)       run_one ;;
    baseline)  build_baseline_bundle ;;
    score)     score_bundle ;;
    finalize)  finalize_bundle ;;
    *)         usage; exit 2 ;;
esac
