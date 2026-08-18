#!/usr/bin/env bash

# Collect the local, same-seed baseline on the container evaluator's reserved
# 10-evaluation-core/2-management-core boundary. This is directional evidence:
# official qualification still requires the authoritative host controls and
# the remaining frontier/independent-seed/reference-patch gates in FINALIZE.md.

set -Eeuo pipefail
umask 077

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
readonly SERVER_DIR="$(cd "$SCRIPT_DIR/../.." && pwd -P)"
readonly EVALUATOR="$SERVER_DIR/competition/container/evaluator.sh"
readonly RESOURCE_BOUNDARY="$SERVER_DIR/competition/container/resource-boundary.sh"
readonly ANCHOR_DIR="$SCRIPT_DIR/eval-12c/container-evaluator-p1800-attempt-011"
readonly ANCHOR_REQUEST="$ANCHOR_DIR/worker-request.json"
readonly CAMPAIGN_DIR="$SCRIPT_DIR/eval-12c/reserved-boundary-same-seed-local"
readonly LOCK_PATH="$SCRIPT_DIR/eval-12c/.reserved-boundary-same-seed-local.lock"
readonly TARGET_BASELINE_REPLICATES=20
readonly EXPECTED_BASE_IMAGE_ID=sha256:22547cd4e19214b1f4688f0eb969d57c70b3f7dc47e02ad9647c4faf7b16a296
readonly EXPECTED_BASE_SHA=2d7881e688c153e1b093c89b4305305465c92395
readonly EXPECTED_SIMULATOR_SHA256=216f6985e7152d38a926ba3c766a6fced156ae4b157e89c566209136ad244176
readonly EXPECTED_PROVIDERS_SHA256=090b3931275d835e7d3166bb1833d221ba41751eec6a682d68aae805b951f138
readonly EXPECTED_ROUND_SEED_HEX=4848484848484848484848484848484848484848484848484848484848484848

# Attempt 11 contributes the first pristine replicate. The production
# evaluator accepts odd replicate counts in 1..9, so 9 + 9 + 1 completes 20.
readonly BATCH_SPECS=("12:9" "13:9" "14:1")

active_evaluator_pid=""

log() {
    printf '[reserved-baseline] %s %s\n' "$(date -u '+%F %T UTC')" "$*"
}

die() {
    log "ERROR: $*" >&2
    exit 1
}

cleanup() {
    local rc=$?
    if [ -n "${active_evaluator_pid:-}" ] && kill -0 "$active_evaluator_pid" 2>/dev/null; then
        log "forwarding termination to evaluator pid $active_evaluator_pid"
        kill -TERM "$active_evaluator_pid" 2>/dev/null || true
        wait "$active_evaluator_pid" 2>/dev/null || true
    fi
    exit "$rc"
}
trap cleanup EXIT INT TERM

sha256_file() {
    sha256sum "$1" | awk '{print $1}'
}

file_bytes() {
    stat -c '%s' "$1"
}

verify_result() {
    local dir="$1" expected_attempt="$2" expected_replicates="$3"
    local request="$dir/worker-request.json"
    local result="$dir/worker-result.json"
    local complete="$dir/evaluation.complete.json"
    local baseline="$dir/baseline.json"
    local evidence_manifest="$dir/evidence-manifest.json"

    for path in "$request" "$result" "$complete" "$baseline" "$evidence_manifest"; do
        [ -s "$path" ] || return 1
    done
    jq -e \
        --argjson attempt "$expected_attempt" \
        --argjson replicates "$expected_replicates" \
        --arg base_image "$EXPECTED_BASE_IMAGE_ID" \
        --arg base_sha "$EXPECTED_BASE_SHA" \
        --arg simulator_sha "$EXPECTED_SIMULATOR_SHA256" \
        --arg providers_sha "$EXPECTED_PROVIDERS_SHA256" \
        --arg round_seed "$EXPECTED_ROUND_SEED_HEX" \
        '.schema == 1 and .attempt == $attempt and
         .evaluation_policy.replicates == $replicates and
         .evaluator_image_digest == $base_image and .base_sha == $base_sha and
         .evaluation_policy.simulator_sha256 == $simulator_sha and
         .evaluation_policy.scorer_sha256 == $simulator_sha and
         .providers_sha256 == $providers_sha and .round_seed_hex == $round_seed' \
        "$request" >/dev/null || return 1
    jq -e \
        --argjson replicates "$expected_replicates" \
        --arg providers_sha "$EXPECTED_PROVIDERS_SHA256" \
        '.score_schema == 1 and .kind == "sim-latency-score-baseline" and
         .config_sha256 == $providers_sha and
         (.replicates | type == "array" and length == $replicates) and
         ([.replicates[] |
           (.raw_score | type == "number" and isfinite and . >= 0) and
           (.success_rate | type == "number" and isfinite and . >= 0.97 and . <= 1) and
           (.request_count | type == "number" and . > 0) and
           (.received_bytes | type == "number" and . > 0) and
           (.findproviders_load_p95_ms | type == "number" and isfinite and . >= 0) and
           (.findproviders_pool_p05 | type == "number" and . > 0)] | all)' \
        "$baseline" >/dev/null || return 1
    jq -e \
        '.schema == 1 and .eval_error == null and .score.placeable == true and
         ([.score.gates[] | .passed == true] | all) and
         ([.security | to_entries[] |
           select(.value | type == "boolean") | .value == true] | all)' \
        "$result" >/dev/null || return 1
    jq -e \
        --argjson attempt "$expected_attempt" \
        --arg base_image "$EXPECTED_BASE_IMAGE_ID" \
        --arg providers_sha "$EXPECTED_PROVIDERS_SHA256" \
        '.schema == 1 and .attempt == $attempt and .cleanup_complete == true and
         .base_image_id == $base_image and .providers_sha256 == $providers_sha' \
        "$complete" >/dev/null || return 1

    local relative expected_sha expected_bytes path
    while IFS=$'\t' read -r relative expected_sha expected_bytes; do
        case "$relative" in
            evidence/*) ;;
            *) return 1 ;;
        esac
        case "/$relative/" in
            *'/../'*|*'/./'*) return 1 ;;
        esac
        path="$dir/$relative"
        [ -f "$path" ] && [ ! -L "$path" ] || return 1
        [ "$(file_bytes "$path")" = "$expected_bytes" ] || return 1
        [ "$(sha256_file "$path")" = "$expected_sha" ] || return 1
    done < <(jq -er '.artifacts[] | [.path,.sha256,(.bytes|tostring)] | @tsv' "$evidence_manifest")
}

write_campaign_manifest() {
    local pending="$CAMPAIGN_DIR/.campaign.json.new"
    jq -n \
        --arg generated_at "$(date -u '+%FT%TZ')" \
        --arg anchor "${ANCHOR_DIR#"$SCRIPT_DIR/"}" \
        --arg campaign "${CAMPAIGN_DIR#"$SCRIPT_DIR/"}" \
        --arg base_image_id "$EXPECTED_BASE_IMAGE_ID" \
        --arg base_sha "$EXPECTED_BASE_SHA" \
        --arg simulator_sha256 "$EXPECTED_SIMULATOR_SHA256" \
        --arg providers_sha256 "$EXPECTED_PROVIDERS_SHA256" \
        --arg round_seed_hex "$EXPECTED_ROUND_SEED_HEX" \
        --argjson target "$TARGET_BASELINE_REPLICATES" \
        '{schema:1,kind:"sim-latency-reserved-boundary-same-seed-campaign",
          classification:"local_directional_not_production_qualified",
          generated_at:$generated_at,anchor_attempt_directory:$anchor,
          campaign_directory:$campaign,target_baseline_replicates:$target,
          identity:{base_image_id:$base_image_id,base_sha:$base_sha,
            simulator_sha256:$simulator_sha256,providers_sha256:$providers_sha256,
            round_seed_hex:$round_seed_hex,evaluation_physical_cores:10,
            management_physical_cores:2,runner_memory_limit_bytes:77309411328},
          batches:[
            {attempt:11,replicates:1,source:"anchor"},
            {attempt:12,replicates:9,source:"campaign"},
            {attempt:13,replicates:9,source:"campaign"},
            {attempt:14,replicates:1,source:"campaign"}
          ]}' > "$pending"
    chmod 0400 "$pending"
    mv -f -- "$pending" "$CAMPAIGN_DIR/campaign.json"
}

prepare_request() {
    local dir="$1" attempt="$2" replicates="$3"
    local patch="$dir/canonical.patch"
    local request="$dir/worker-request.json"
    local pending="$request.new"
    local job_id

    job_id="$(< /proc/sys/kernel/random/uuid)"
    install -m 0400 "$ANCHOR_DIR/canonical.patch" "$patch"
    jq \
        --arg job_id "$job_id" \
        --argjson attempt "$attempt" \
        --arg artifact_directory "$dir" \
        --arg patch_path "$patch" \
        --argjson replicates "$replicates" \
        '.job_id = $job_id |
         .attempt = $attempt |
         .competition_id = "container-local-reserved-boundary-same-seed" |
         .artifact_directory = $artifact_directory |
         .patch_path = $patch_path |
         .evaluation_policy.replicates = $replicates' \
        "$ANCHOR_REQUEST" > "$pending"
    chmod 0400 "$pending"
    mv -f -- "$pending" "$request"
    [ "$(sha256_file "$patch")" = "$(jq -er '.patch_sha256' "$request")" ] ||
        die "attempt $attempt patch identity mismatch"
}

run_attempt() {
    local attempt="$1" replicates="$2"
    local ordinal dir result evaluator_log start_s elapsed_s stage containers
    ordinal="$(printf '%03d' "$attempt")"
    dir="$CAMPAIGN_DIR/attempt-$ordinal"
    result="$dir/worker-result.json"
    evaluator_log="$dir/evaluator.log"

    if verify_result "$dir" "$attempt" "$replicates"; then
        log "attempt $attempt already authenticated ($replicates pristine replicates); skipping"
        return
    fi
    if [ -e "$dir" ]; then
        die "attempt $attempt has partial or invalid artifacts; preserving them at $dir"
    fi
    install -d -m 0700 "$dir"
    prepare_request "$dir" "$attempt" "$replicates"

    log "attempt $attempt starting: $replicates pristine + $replicates comment-only A/A replicates"
    start_s="$(date +%s)"
    "$EVALUATOR" --request "$dir/worker-request.json" --result "$result" \
        > "$evaluator_log" 2>&1 &
    active_evaluator_pid=$!
    while kill -0 "$active_evaluator_pid" 2>/dev/null; do
        sleep 30
        elapsed_s=$(($(date +%s) - start_s))
        stage="$(sudo -n docker ps --filter "label=com.urnetwork.competition.job-id=$(jq -r '.job_id' "$dir/worker-request.json")" \
            --format '{{.Label "com.urnetwork.competition.stage"}}' | sort -u | paste -sd, -)"
        containers="$(sudo -n docker ps --filter "label=com.urnetwork.competition.job-id=$(jq -r '.job_id' "$dir/worker-request.json")" -q | wc -l)"
        log "attempt $attempt elapsed=${elapsed_s}s stage=${stage:-between-stages} active_containers=$containers"
    done
    if wait "$active_evaluator_pid"; then
        evaluator_rc=0
    else
        evaluator_rc=$?
    fi
    active_evaluator_pid=""
    [ "$evaluator_rc" -eq 0 ] || {
        tail -n 80 "$evaluator_log" >&2 || true
        die "attempt $attempt evaluator exited $evaluator_rc; artifacts preserved"
    }
    verify_result "$dir" "$attempt" "$replicates" ||
        die "attempt $attempt completed but its authenticated result failed verification"
    log "attempt $attempt complete and authenticated"
}

for command in awk date flock install jq paste realpath sha256sum sort stat sudo; do
    command -v "$command" >/dev/null 2>&1 || die "required command missing: $command"
done
[ -x "$EVALUATOR" ] || die "trusted evaluator is not executable: $EVALUATOR"
[ -x "$RESOURCE_BOUNDARY" ] || die "resource-boundary helper is not executable"
[ -s "$ANCHOR_REQUEST" ] || die "authenticated attempt-11 anchor is missing"
[ "$(jq -er '.evaluator_image_digest' "$ANCHOR_REQUEST")" = "$EXPECTED_BASE_IMAGE_ID" ] ||
    die "anchor base image identity changed"
[ "$(jq -er '.base_sha' "$ANCHOR_REQUEST")" = "$EXPECTED_BASE_SHA" ] ||
    die "anchor base SHA changed"
[ "$(jq -er '.providers_sha256' "$ANCHOR_REQUEST")" = "$EXPECTED_PROVIDERS_SHA256" ] ||
    die "anchor provider identity changed"
[ "$(jq -er '.round_seed_hex' "$ANCHOR_REQUEST")" = "$EXPECTED_ROUND_SEED_HEX" ] ||
    die "anchor round seed changed"
verify_result "$ANCHOR_DIR" 11 1 || die "attempt-11 anchor failed authentication"

resource_boundary_json="$($RESOURCE_BOUNDARY)"
jq -e \
    '.evaluation_physical_core_count == 10 and
     .management_physical_core_count == 2 and
     .runner_memory_limit_bytes == 77309411328 and
     .disjoint_cpu_sets == true and .memory_capacity_passed == true' \
    <<<"$resource_boundary_json" >/dev/null || die "live resource boundary is not the reserved 10+2 policy"
sudo -n docker info >/dev/null
[ -z "$(sudo -n docker ps -aq --filter label=com.urnetwork.competition.job-id)" ] ||
    die "another competition evaluation has residual containers"

install -d -m 0700 "$CAMPAIGN_DIR"
exec 9>"$LOCK_PATH"
flock -n 9 || die "another reserved-boundary campaign supervisor holds $LOCK_PATH"
write_campaign_manifest

for spec in "${BATCH_SPECS[@]}"; do
    attempt="${spec%%:*}"
    replicates="${spec##*:}"
    run_attempt "$attempt" "$replicates"
done

log "campaign data collection complete: $TARGET_BASELINE_REPLICATES pristine same-seed replicates"
