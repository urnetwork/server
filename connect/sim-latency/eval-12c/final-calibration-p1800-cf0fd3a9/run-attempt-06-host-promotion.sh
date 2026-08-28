#!/usr/bin/env bash

# Requalify the unchanged host boundary for the corrected evaluator image.
# The evaluator runs before any active host state changes. Runtime markers and
# the host config are replaced only after a complete, placeable p1800 A/A pair;
# every old root-owned marker is retained and restored on a failed promotion.

set -Eeuo pipefail
umask 077
export LANG=C LC_ALL=C TZ=UTC

readonly SERVER=/home/by/urnetwork/server
readonly EVIDENCE=/home/by/urnetwork/server-finalization-evidence
readonly ROOT="$EVIDENCE/connect/sim-latency/eval-12c/final-calibration-p1800-cf0fd3a9"
readonly HISTORICAL_ROOT="$SERVER/connect/sim-latency/eval-12c/final-calibration-p1800-cf0fd3a9"
readonly ATTEMPT_ROOT=/var/lib/urnetwork/competition-host-requalification-2abcf145
readonly ATTEMPT="$ATTEMPT_ROOT/attempt-01"
readonly ANCHOR="$HISTORICAL_ROOT/qualification-a-a/attempt-12001/worker-request.json"
readonly EVALUATOR="$EVIDENCE/competition/container/evaluator.sh"
readonly PROMOTER="$EVIDENCE/competition/promote-host-containment.sh"
readonly HOST_CHECK="$EVIDENCE/competition/host-self-check.sh"
readonly REFERENCE_PATCH="$SERVER/competition/references/noop.patch"
readonly SOURCE_LOCK="$ROOT/source-lock.json"
readonly RESOURCE_BOMB="$ROOT/resource-bomb-cleanup-production-2abcf145.json"
readonly PENDING_HOST_CONFIG="$ROOT/competition-host.2abcf145-pending.json"
readonly RUNTIME_PENDING_HOST_CONFIG=/run/urnetwork/competition-host.2abcf145-pending.json
readonly HOST_CONFIG=/etc/urnetwork/competition-host.json
readonly STATE_BACKUP=/var/lib/urnetwork/competition-host-state-cf0fd3a9
readonly SUMMARY="$ROOT/host-promotion-attempt-06.json"
readonly PROMOTION="$ROOT/containment-promotion-attempt-06.json"
readonly REBASELINE="$ROOT/rebaseline-attempt-06.json"
readonly SELF_CHECK="$ROOT/host-self-check-attempt-06.json"
readonly BASE_SHA=46515d82fe98ff666c61b2b5bb1d34a89cf4dad8
readonly BASE_IMAGE=sha256:2abcf145c0f914899debbd2fd52e57a16cf20072165c8d13f04a0ba487198a4c
readonly SIMULATOR_SHA=bc843ce2b9cdcc41459362c7a682b08e7a12a8ac896443fe1e8aad94d4b17997
readonly SOURCE_LOCK_SHA=94c25024a92b5fcb5fa8bf324ff8022fde1074fd62bc210fc0ad5efbba0e4022
readonly HOST_QUALIFICATION_SHA=acf226db6b8e50d67f8957cddb3903d5d4e9e82566935d61d270ccb5b03463a3
readonly PENDING_HOST_CONFIG_SHA=3ad8586dc3bf50076fa17703115088a6f79410b95ce502f0ca30026d770c2af1
readonly RESOURCE_BOMB_SHA=08296c7ff35edefb09eae50cd9e415dad86ae37d4347a64022da3d5dcfa841bc
readonly PATCH_SHA=8bd57a48ac82a6e846b607a9301c48145da5c66717c9e3a341138d034d1e0775
readonly PREVIOUS_HOST_CONFIG_SHA=6a0421724d86c4a05f06ce82e79c5e4eb8abf2a29a1ea0a694d79cbf713be98c
readonly BOOT_ID=34760d1b-a0b6-46a0-b8c1-264abd1affba
readonly MANAGEMENT_CPUS=20,22

readonly -a MARKERS=(
    /run/urnetwork/template-database.json
    /run/urnetwork/redis-reset.json
    /run/urnetwork/cleanup.json
    /run/urnetwork/immutable-reports.json
    /run/urnetwork/rebaseline.json
)
readonly -a MARKER_HASHES=(
    ab0383e72f9b7f2465f9a39e42af5c8e5794434433c556faf488d09eff6bf8f0
    edc32d381e740a5b370ad45a49fae09eba406551f2e2ff75e385984f0bb3be11
    d918447ff4803278969ea06d4f9d0877568db2e6d44b712e0e012596c3bcab93
    a098d6694b8d37439728a9d8300fff1efeeacf4b9b7c7cb53667567d7375a604
    197965b224ca995e3a480643308026b7c59c8bc898a09fb0ac3ac242f20323b7
)

active_pid=""
state_backed_up=false
promotion_committed=false
preflight_only=false
resume_only=false
verify_only=false

log() { printf '[host-promotion-2abcf145] %s %s\n' "$(date -u '+%FT%TZ')" "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }
sha256_file() { sha256sum "$1" | awk '{print $1}'; }

restore_previous_state() {
    local index marker backup
    [ "$state_backed_up" = true ] || return 0
    log "restoring the authenticated cf0fd3a9 host state"
    sudo -n install -o root -g root -m 0600 "$STATE_BACKUP/competition-host.json" "$HOST_CONFIG"
    for index in "${!MARKERS[@]}"; do
        marker="${MARKERS[$index]}"
        backup="$STATE_BACKUP/${marker##*/}"
        sudo -n install -o root -g root -m 0600 "$backup" "$marker"
    done
    sync
}

cleanup() {
    local rc=$?
    trap - EXIT INT TERM
    if [ -n "${active_pid:-}" ] && kill -0 "$active_pid" 2>/dev/null; then
        kill -TERM "$active_pid" 2>/dev/null || true
        wait "$active_pid" 2>/dev/null || true
    fi
    if [ "$promotion_committed" != true ]; then
        restore_previous_state || rc=1
    fi
    exit "$rc"
}
trap cleanup EXIT INT TERM

for command in awk chmod date docker find flock git install jq kill mv paste readlink sha256sum sleep sort stat sudo sync tail taskset unshare wc; do
    command -v "$command" >/dev/null 2>&1 || die "missing command: $command"
done
case "${1:-}" in
    --preflight) [ "$#" -eq 1 ] || die "usage: $0 [--preflight|--verify-attempt|--resume]"; preflight_only=true ;;
    --verify-attempt) [ "$#" -eq 1 ] || die "usage: $0 [--preflight|--verify-attempt|--resume]"; verify_only=true ;;
    --resume) [ "$#" -eq 1 ] || die "usage: $0 [--preflight|--verify-attempt|--resume]"; resume_only=true ;;
    "") [ "$#" -eq 0 ] || die "usage: $0 [--preflight|--verify-attempt|--resume]" ;;
    *) die "usage: $0 [--preflight|--verify-attempt|--resume]" ;;
esac
[ "$(id -u)" -ne 0 ] || die "run as the evaluator operator, not root"
[ "$(< /proc/sys/kernel/random/boot_id)" = "$BOOT_ID" ] || die "host rebooted"
[ "$(git -C "$SERVER" rev-parse HEAD)" = "$BASE_SHA" ] || die "server source changed"
[ -z "$(git -C "$SERVER" status --porcelain --untracked-files=no)" ] || die "server tracked worktree changed"
[ "$(sha256_file "$SOURCE_LOCK")" = "$SOURCE_LOCK_SHA" ] || die "source lock changed"
[ "$(sha256_file "$PENDING_HOST_CONFIG")" = "$PENDING_HOST_CONFIG_SHA" ] || die "pending host config changed"
[ "$(sha256_file "$RESOURCE_BOMB")" = "$RESOURCE_BOMB_SHA" ] || die "resource-bomb report changed"
[ "$(sha256_file "$REFERENCE_PATCH")" = "$PATCH_SHA" ] || die "no-op patch changed"
git -C "$SERVER" apply --check "$REFERENCE_PATCH" || die "no-op patch no longer applies"
[ "$(sudo -n docker image inspect --format '{{.Id}}' "$BASE_IMAGE")" = "$BASE_IMAGE" ] ||
    die "replacement evaluator image is unavailable"
[ -s "$ANCHOR" ] || die "p1800 request anchor is unavailable"
[ -x "$EVALUATOR" ] && [ -x "$PROMOTER" ] && [ -x "$HOST_CHECK" ] || die "trusted host tools are unavailable"
[ "$(sudo -n sha256sum "$HOST_CONFIG" | awk '{print $1}')" = "$PREVIOUS_HOST_CONFIG_SHA" ] ||
    die "active host config is not the authenticated cf0fd3a9 state"
sudo -n jq -e '.image_digest == "sha256:cf0fd3a9e73385729ee8dcd8da7ea53eb59d5f372b9ff36789ec923056222038"' \
    "$HOST_CONFIG" >/dev/null || die "active host config image is unexpected"
for index in "${!MARKERS[@]}"; do
    [ "$(sudo -n sha256sum "${MARKERS[$index]}" | awk '{print $1}')" = "${MARKER_HASHES[$index]}" ] ||
        die "active runtime marker changed: ${MARKERS[$index]}"
done
[ -z "$(sudo -n docker ps -aq --filter label=com.urnetwork.competition.job-id)" ] ||
    die "another evaluation is active"
[ -z "$(sudo -n docker ps -aq --filter 'name=^/urnetwork-local-pg$')" ] || die "local PostgreSQL is active"
[ -z "$(sudo -n docker ps -aq --filter 'name=^/urnetwork-local-redis$')" ] || die "local Redis is active"
[ ! -e "$STATE_BACKUP" ] || die "previous host-state archive already exists"
[ ! -e "$SUMMARY" ] && [ ! -e "$PROMOTION" ] && [ ! -e "$REBASELINE" ] && [ ! -e "$SELF_CHECK" ] ||
    die "host-promotion evidence already exists"
if [ "$preflight_only" = true ]; then
    [ ! -e "$ATTEMPT_ROOT" ] || die "host requalification attempt already exists"
    log "preflight passed: replacement image, pending host identity, hostile-job proof, and rollback state are authenticated"
    promotion_committed=true
    exit 0
fi

if [ "$resume_only" = true ] || [ "$verify_only" = true ]; then
    [ -d "$ATTEMPT" ] || die "completed host requalification attempt is unavailable"
    job_id="$(jq -er '.job_id' "$ATTEMPT/worker-request.json")"
    round_id="$(jq -er '.round_id' "$ATTEMPT/worker-request.json")"
    start_epoch="$(stat -c '%Y' "$ATTEMPT/worker-request.json")"
    log "resuming promotion from completed pair: job=$job_id round=$round_id"
else
    [ ! -e "$ATTEMPT_ROOT" ] || die "host requalification attempt already exists"
sudo -n install -d -o "$(id -u)" -g "$(id -g)" -m 0700 "$ATTEMPT_ROOT" "$ATTEMPT"
install -m 0400 "$REFERENCE_PATCH" "$ATTEMPT/canonical.patch"
job_id="$(< /proc/sys/kernel/random/uuid)"
round_id="$(< /proc/sys/kernel/random/uuid)"
config_sha="$(jq -er '.config_local_sha256' "$PENDING_HOST_CONFIG")"
vault_sha="$(jq -er '.vault_local_sha256' "$PENDING_HOST_CONFIG")"
jq \
    --arg job_id "$job_id" --arg round_id "$round_id" \
    --arg artifact_directory "$ATTEMPT" --arg patch_path "$ATTEMPT/canonical.patch" \
    --arg patch_sha "$PATCH_SHA" --arg base_sha "$BASE_SHA" --arg base_image "$BASE_IMAGE" \
    --arg simulator_sha "$SIMULATOR_SHA" --arg host_qualification "$HOST_QUALIFICATION_SHA" \
    --arg config_sha "$config_sha" --arg vault_sha "$vault_sha" \
    '.job_id = $job_id | .round_id = $round_id | .attempt = 1 |
     .competition_id = "sim-latency-p1800-attempt-06-host-promotion" |
     .artifact_directory = $artifact_directory | .patch_path = $patch_path |
     .patch_sha256 = $patch_sha | .base_sha = $base_sha |
     .evaluator_image_digest = $base_image |
     .evaluation_policy.host_qualification_sha256 = $host_qualification |
     .evaluation_policy.simulator_sha256 = $simulator_sha |
     .evaluation_policy.scorer_sha256 = $simulator_sha |
     .evaluation_policy.provider_count = 1800 |
     .evaluation_policy.client_pool_size = 200 |
     .evaluation_policy.arrivals_per_minute = 80 |
     .evaluation_policy.quality_window_size = 2 |
     .evaluation_policy.exchange_hosts = 4 |
     .evaluation_policy.fleet_shards = 4 |
     .evaluation_policy.ramp_ms = 60000 |
     .evaluation_policy.prewarm_ms = 46800000 |
     .evaluation_policy.settle_ms = 60000 |
     .evaluation_policy.client_warmup_timeout_ms = 1200000 |
     .evaluation_policy.duration_ms = 180000 |
     .evaluation_policy.request_timeout_ms = 120000 |
     .evaluation_policy.impairment_enabled = true |
     .evaluation_policy.replicates = 1 |
     .evaluation_policy.takeover_margin = 0.5 |
     .evaluation_policy.score_timeout_seconds = 8000 |
     .evaluation_policy.config_local_sha256 = $config_sha |
     .evaluation_policy.vault_local_sha256 = $vault_sha' \
    "$ANCHOR" >"$ATTEMPT/worker-request.json.new"
chmod 0400 "$ATTEMPT/worker-request.json.new"
mv "$ATTEMPT/worker-request.json.new" "$ATTEMPT/worker-request.json"

log "starting one p1800 baseline/no-op host-promotion pair"
start_epoch="$(date +%s)"
taskset -c "$MANAGEMENT_CPUS" "$EVALUATOR" \
    --request "$ATTEMPT/worker-request.json" --result "$ATTEMPT/worker-result.json" \
    >"$ATTEMPT/evaluator.log" 2>&1 &
active_pid=$!
while kill -0 "$active_pid" 2>/dev/null; do
    sleep 30
    elapsed=$(( $(date +%s) - start_epoch ))
    stage="$(sudo -n docker ps --filter "label=com.urnetwork.competition.job-id=$job_id" \
        --format '{{.Label "com.urnetwork.competition.stage"}}' | sort -u | paste -sd, -)"
    containers="$(sudo -n docker ps --filter "label=com.urnetwork.competition.job-id=$job_id" -q | wc -l)"
    log "elapsed=${elapsed}s stage=${stage:-between-stages} active_containers=$containers"
done
if wait "$active_pid"; then evaluator_rc=0; else evaluator_rc=$?; fi
active_pid=""
if [ "$evaluator_rc" -ne 0 ]; then
    tail -n 160 "$ATTEMPT/evaluator.log" >&2 || true
    die "host-promotion evaluator exited $evaluator_rc"
fi
fi
for artifact in worker-result.json baseline.json score.json resources.json accounting.json evidence-manifest.json evaluation.complete.json; do
    [ -s "$ATTEMPT/$artifact" ] || die "missing evaluator artifact: $artifact"
done
jq -e --arg job "$job_id" \
    '.job_id == $job and .eval_error == null and .score.score_schema == 1 and
     .score.placeable == true and ([.score.gates[].passed] | all) and
     ([.security | to_entries[] | select(.value | type == "boolean") | .value] | all)' \
    "$ATTEMPT/worker-result.json" >/dev/null || die "host-promotion pair did not pass"
jq -e --arg image "$BASE_IMAGE" --arg job "$job_id" --arg round "$round_id" --arg patch "$PATCH_SHA" \
    '.base_image_id == $image and .job_id == $job and .round_id == $round and
     .patch_sha256 == $patch and .cleanup_complete == true' \
    "$ATTEMPT/evaluation.complete.json" >/dev/null || die "host-promotion completion identity is invalid"
jq -e --arg image "$BASE_IMAGE" --arg job "$job_id" --arg round "$round_id" \
    --arg patch "$PATCH_SHA" --arg base "$BASE_SHA" --arg host "$HOST_QUALIFICATION_SHA" \
    '.job_id == $job and .round_id == $round and .patch_sha256 == $patch and
     .base_sha == $base and .evaluator_image_digest == $image and
     .evaluation_policy.host_qualification_sha256 == $host' \
    "$ATTEMPT/worker-request.json" >/dev/null || die "host-promotion request identity is invalid"
[ "$(sha256_file "$ATTEMPT/canonical.patch")" = "$PATCH_SHA" ] || die "host-promotion patch changed"
for artifact in baseline.json score.json resources.json accounting.json evidence-manifest.json evaluation.complete.json; do
    expected_sha="$(jq -er --arg path "$artifact" \
        '[.artifacts[] | select(.path == $path) | .sha256] | if length == 1 then .[0] else error("artifact digest missing") end' \
        "$ATTEMPT/worker-result.json")"
    [ "$(sha256_file "$ATTEMPT/$artifact")" = "$expected_sha" ] || die "artifact digest changed: $artifact"
done
[ -z "$(sudo -n docker ps -aq --filter "label=com.urnetwork.competition.job-id=$job_id")" ] ||
    die "containers remain after host-promotion evaluation"
[ -z "$(sudo -n docker network ls -q --filter "label=com.urnetwork.competition.job-id=$job_id")" ] ||
    die "networks remain after host-promotion evaluation"
if [ "$verify_only" = true ]; then
    log "completed pair passed immutable artifact-contract verification"
    promotion_committed=true
    exit 0
fi

log "backing up the active host config and runtime markers"
sudo -n install -d -o root -g root -m 0700 "$STATE_BACKUP"
sudo -n install -o root -g root -m 0400 "$HOST_CONFIG" "$STATE_BACKUP/competition-host.json"
for marker in "${MARKERS[@]}"; do
    sudo -n install -o root -g root -m 0400 "$marker" "$STATE_BACKUP/${marker##*/}"
done
state_backed_up=true
sudo -n chmod 0500 "$STATE_BACKUP"
sudo -n install -o root -g root -m 0600 "$PENDING_HOST_CONFIG" "$RUNTIME_PENDING_HOST_CONFIG"

log "promoting containment markers against the replacement image"
sudo -n "$PROMOTER" --host-config "$RUNTIME_PENDING_HOST_CONFIG" \
    --evaluation-dir "$ATTEMPT" --resource-bomb-report "$RESOURCE_BOMB" \
    >"$PROMOTION.new"
chmod 0400 "$PROMOTION.new"
mv "$PROMOTION.new" "$PROMOTION"

jq -n --arg promoted_at "$(date -u '+%FT%TZ')" --arg round_id "$round_id" \
    --arg job_id "$job_id" --arg image "$BASE_IMAGE" \
    --arg baseline_sha "$(sha256_file "$ATTEMPT/baseline.json")" \
    --arg manifest_sha "$(sha256_file "$ATTEMPT/evidence-manifest.json")" \
    '{schema:1,kind:"sim-latency-round-rebaseline",promoted_at:$promoted_at,
      round_id:$round_id,job_id:$job_id,image_digest:$image,
      baseline_sha256:$baseline_sha,evidence_manifest_sha256:$manifest_sha,passed:true}' \
    >"$REBASELINE.new"
chmod 0400 "$REBASELINE.new"
mv "$REBASELINE.new" "$REBASELINE"
rebaseline_sha="$(sha256_file "$REBASELINE")"
sudo -n install -o root -g root -m 0600 "$REBASELINE" /run/urnetwork/rebaseline.json
jq --arg marker_sha "$rebaseline_sha" '.rebaseline_manifest_sha256 = $marker_sha' \
    "$RUNTIME_PENDING_HOST_CONFIG" >"$ROOT/competition-host.2abcf145-promoted.json"
chmod 0400 "$ROOT/competition-host.2abcf145-promoted.json"
sudo -n install -o root -g root -m 0600 "$ROOT/competition-host.2abcf145-promoted.json" \
    "$RUNTIME_PENDING_HOST_CONFIG"

log "validating the complete replacement host state before activation"
set +e
pending_report="$(sudo -n unshare --mount /bin/bash -c \
    'mount --bind "$1" /etc/urnetwork/competition-host.json; exec taskset -c "$2" "$3" --json' \
    bash "$RUNTIME_PENDING_HOST_CONFIG" "$MANAGEMENT_CPUS" "$HOST_CHECK")"
pending_rc=$?
set -e
[ "$pending_rc" -eq 0 ] || die "replacement host state did not pass isolated self-check"
jq -e --arg image "$BASE_IMAGE" --arg qualification "$HOST_QUALIFICATION_SHA" --arg round "$round_id" \
    '.image_digest == $image and .qualification_sha256 == $qualification and
     .rebaseline_passed == true and .rebaseline_round_id == $round and
     .cleanup_verified == true and .resource_limits_verified == true and
     .resource_bomb_cleanup_verified == true and .services_in_job_cgroup == true and
     .default_deny_network == true and .no_production_secrets == true and
     ([.checks[]] | all)' <<<"$pending_report" >/dev/null || die "isolated replacement self-check is invalid"

sudo -n install -o root -g root -m 0600 "$RUNTIME_PENDING_HOST_CONFIG" "$HOST_CONFIG"
active_report="$(sudo -n taskset -c "$MANAGEMENT_CPUS" "$HOST_CHECK" --json)"
[ "$active_report" = "$pending_report" ] || die "active host state differs from the validated replacement"
printf '%s\n' "$active_report" >"$SELF_CHECK.new"
chmod 0400 "$SELF_CHECK.new"
mv "$SELF_CHECK.new" "$SELF_CHECK"

jq -n --arg completed_at "$(date -u '+%FT%TZ')" --arg job "$job_id" --arg round "$round_id" \
    --arg source "$SOURCE_LOCK_SHA" --arg image "$BASE_IMAGE" --arg qualification "$HOST_QUALIFICATION_SHA" \
    --arg request "$(sha256_file "$ATTEMPT/worker-request.json")" \
    --arg worker "$(sha256_file "$ATTEMPT/worker-result.json")" \
    --arg manifest "$(sha256_file "$ATTEMPT/evidence-manifest.json")" \
    --arg promotion "$(sha256_file "$PROMOTION")" --arg rebaseline "$rebaseline_sha" \
    --arg self_check "$(sha256_file "$SELF_CHECK")" --arg resource_bomb "$RESOURCE_BOMB_SHA" \
    --argjson elapsed "$(( $(date +%s) - start_epoch ))" \
    --argjson baseline_score "$(jq -er '.replicates[0].raw_score' "$ATTEMPT/baseline.json")" \
    --argjson candidate_score "$(jq -er '.score.raw_score' "$ATTEMPT/worker-result.json")" \
    '{schema:1,kind:"sim-latency-attempt-06-host-promotion",completed_at:$completed_at,
      passed:true,job_id:$job,round_id:$round,source_lock_sha256:$source,
      evaluator_image_digest:$image,host_qualification_sha256:$qualification,
      scale:{provider_count:1800,client_pool_size:200,arrivals_per_minute:80,
        quality_window_size:2,exchange_hosts:4,fleet_shards:4,duration_ms:180000,
        impairment_enabled:true,replicates_per_side:1},elapsed_seconds:$elapsed,
      baseline_raw_score_ms:$baseline_score,candidate_raw_score_ms:$candidate_score,
      evidence_sha256:{request:$request,worker_result:$worker,evidence_manifest:$manifest,
        containment_promotion:$promotion,rebaseline:$rebaseline,self_check:$self_check,
        resource_bomb:$resource_bomb},
      assertions:{all_six_gates_passed:true,cleanup_complete:true,
        fresh_cpu_memory_bomb_cleanup:true,isolated_self_check_passed:true,
        active_self_check_identical:true,previous_state_retained:true}}' >"$SUMMARY.new"
chmod 0400 "$SUMMARY.new"
mv "$SUMMARY.new" "$SUMMARY"

promotion_committed=true
sudo -n chmod 0500 "$STATE_BACKUP"
log "host promotion passed: job=$job_id round=$round_id elapsed=$(( $(date +%s) - start_epoch ))s"
printf '%s %s\n' "$job_id" "$round_id"
