#!/usr/bin/env bash

# Promote an authenticated competitionrebaseline evaluation into the
# root-owned same-round readiness marker. The ordinary worker must be stopped
# and the host single-job operational lock held by the caller.

set -Eeuo pipefail
umask 077

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
readonly PROMOTE_CONTAINMENT="$SCRIPT_DIR/promote-host-containment.sh"

result=""
host_config=""
resource_bomb_report=""
self_check=""
self_check_sha256=""
output_directory=""

usage() {
    printf '%s\n' \
        'usage: promote-round-rebaseline.sh --result PATH --host-config PATH' \
        '       --resource-bomb-report PATH --self-check PATH' \
        '       --self-check-sha256 HEX --output-directory PATH' >&2
    exit 2
}

die() {
    printf '[competition-rebaseline-promotion] ERROR: %s\n' "$*" >&2
    exit 1
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --result) [ "$#" -ge 2 ] || usage; result="$2"; shift 2 ;;
        --host-config) [ "$#" -ge 2 ] || usage; host_config="$2"; shift 2 ;;
        --resource-bomb-report) [ "$#" -ge 2 ] || usage; resource_bomb_report="$2"; shift 2 ;;
        --self-check) [ "$#" -ge 2 ] || usage; self_check="$2"; shift 2 ;;
        --self-check-sha256) [ "$#" -ge 2 ] || usage; self_check_sha256="$2"; shift 2 ;;
        --output-directory) [ "$#" -ge 2 ] || usage; output_directory="$2"; shift 2 ;;
        *) usage ;;
    esac
done

[ "$(id -u)" -eq 0 ] || die "promotion must run as root"
for value in "$result" "$host_config" "$resource_bomb_report" "$self_check" \
    "$self_check_sha256" "$output_directory"; do
    [ -n "$value" ] || usage
done
for command in awk chmod date dirname find install jq mv realpath sha256sum stat sync taskset; do
    command -v "$command" >/dev/null 2>&1 || die "required command missing: $command"
done
[ -x "$PROMOTE_CONTAINMENT" ] || die "containment promotion helper is unavailable"
[[ "$self_check_sha256" =~ ^[0-9a-f]{64}$ ]] || die "self-check digest is malformed"

sha256_file() {
    sha256sum "$1" | awk '{print $1}'
}

secure_regular() {
    local path="$1" mode
    [ -f "$path" ] && [ ! -L "$path" ] || return 1
    mode="$(stat -c %a "$path")"
    [ $((8#$mode & 0022)) -eq 0 ]
}

secure_root_owned() {
    secure_regular "$1" && [ "$(stat -c %u "$1")" -eq 0 ]
}

result="$(realpath -e -- "$result")"
host_config="$(realpath -e -- "$host_config")"
resource_bomb_report="$(realpath -e -- "$resource_bomb_report")"
self_check="$(realpath -e -- "$self_check")"
output_directory="$(realpath -m -- "$output_directory")"
secure_regular "$result" || die "rebaseline result is unsafe"
secure_root_owned "$host_config" || die "host config is not a secure root-owned file"
secure_regular "$resource_bomb_report" || die "resource-bomb evidence is unsafe"
secure_root_owned "$self_check" || die "self-check is not a secure root-owned executable"
[ -x "$self_check" ] || die "self-check is not executable"
[ "$(sha256_file "$self_check")" = "$self_check_sha256" ] || die "self-check digest mismatch"

case "$output_directory" in /|/etc|/run|/var|/var/lib) die "output directory is too broad" ;; esac
if [ ! -e "$output_directory" ]; then
    parent="$(dirname -- "$output_directory")"
    [ -d "$parent" ] && [ ! -L "$parent" ] && [ "$(stat -c %u "$parent")" -eq 0 ] ||
        die "output parent is unsafe"
    parent_mode="$(stat -c %a "$parent")"
    [ $((8#$parent_mode & 0022)) -eq 0 ] || die "output parent is group/world writable"
    install -d -o 0 -g 0 -m 0700 "$output_directory"
fi
[ -d "$output_directory" ] && [ ! -L "$output_directory" ] &&
    [ "$(stat -c %u "$output_directory")" -eq 0 ] || die "output directory is unsafe"
output_mode="$(stat -c %a "$output_directory")"
[ $((8#$output_mode & 0022)) -eq 0 ] || die "output directory is group/world writable"
[ -z "$(find "$output_directory" -mindepth 1 -maxdepth 1 -print -quit)" ] ||
    die "output directory must be empty"

readonly result_keys='["attempt_directory","base_sha","baseline_sha256","candidate_placeable","evaluation_complete_sha256","evaluator_image_digest","evidence_manifest_sha256","generated_at","job_id","kind","patch_sha256","round_id","schema","worker_artifact_manifest_sha256","worker_result_sha256"]'
jq -e --argjson keys "$result_keys" \
    '.schema == 1 and .kind == "sim-latency-round-rebaseline-evaluation" and
     ((keys | sort) == $keys) and .candidate_placeable == true and
     (.generated_at | type == "string" and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T")) and
     (.attempt_directory | type == "string" and startswith("/")) and
     (.round_id | type == "string" and test("^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")) and
     (.job_id | type == "string" and test("^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")) and
     (.base_sha | test("^[0-9a-f]{40}$")) and
     (.evaluator_image_digest | test("^sha256:[0-9a-f]{64}$")) and
     (.patch_sha256 | test("^[0-9a-f]{64}$")) and
     (.baseline_sha256 | test("^[0-9a-f]{64}$")) and
     (.evidence_manifest_sha256 | test("^[0-9a-f]{64}$")) and
     (.worker_result_sha256 | test("^[0-9a-f]{64}$")) and
     (.evaluation_complete_sha256 | test("^[0-9a-f]{64}$")) and
     (.worker_artifact_manifest_sha256 | test("^[0-9a-f]{64}$"))' \
    "$result" >/dev/null || die "rebaseline result failed schema validation"

declared_attempt_directory="$(jq -er '.attempt_directory' "$result")"
attempt_directory="$(realpath -e -- "$declared_attempt_directory")"
[ "$declared_attempt_directory" = "$attempt_directory" ] ||
    die "rebaseline attempt directory is non-canonical"
[ -d "$attempt_directory" ] && [ ! -L "$attempt_directory" ] ||
    die "rebaseline attempt directory is unsafe"
job_id="$(jq -er '.job_id' "$result")"
round_id="$(jq -er '.round_id' "$result")"
configured_artifact_root="$(jq -er '.artifact_root' "$host_config")"
artifact_root="$(realpath -e -- "$configured_artifact_root")"
[ "$configured_artifact_root" = "$artifact_root" ] && [ -d "$artifact_root" ] &&
    [ ! -L "$artifact_root" ] || die "configured artifact root is unsafe"
[ "$attempt_directory" = "$artifact_root/$job_id/attempt-01" ] ||
    die "attempt path does not match the configured artifact root and job identity"

for declaration in \
    "baseline.json:baseline_sha256" \
    "evidence-manifest.json:evidence_manifest_sha256" \
    "worker-result.json:worker_result_sha256" \
    "evaluation.complete.json:evaluation_complete_sha256"; do
    name="${declaration%%:*}"
    key="${declaration##*:}"
    path="$attempt_directory/$name"
    secure_regular "$path" || die "rebaseline artifact is unsafe: $name"
    expected="$(jq -er --arg key "$key" '.[$key]' "$result")"
    [ "$(sha256_file "$path")" = "$expected" ] || die "rebaseline artifact changed: $name"
done

request="$attempt_directory/worker-request.json"
secure_regular "$request" || die "rebaseline worker request is unsafe"
base_sha="$(jq -er '.base_sha' "$result")"
image_digest="$(jq -er '.evaluator_image_digest' "$result")"
patch_sha256="$(jq -er '.patch_sha256' "$result")"
host_image_digest="$(jq -er '.image_digest' "$host_config")"
[ "$image_digest" = "$host_image_digest" ] || die "rebaseline image does not match the host"
jq -e --arg job_id "$job_id" --arg round_id "$round_id" \
    --arg attempt_directory "$attempt_directory" --arg base_sha "$base_sha" \
    --arg image_digest "$image_digest" --arg patch_sha256 "$patch_sha256" \
    '.schema == 1 and .job_id == $job_id and .round_id == $round_id and
     .attempt == 1 and .artifact_directory == $attempt_directory and
     .base_sha == $base_sha and .evaluator_image_digest == $image_digest and
     .patch_sha256 == $patch_sha256 and
     (.providers_sha256 | test("^[0-9a-f]{64}$")) and
     (.evaluation_policy.replicates | type == "number" and . >= 1 and . <= 9 and (. % 2) == 1)' \
    "$request" >/dev/null || die "rebaseline worker request identity mismatch"

providers_sha256="$(jq -er '.providers_sha256' "$request")"
replicates="$(jq -er '.evaluation_policy.replicates' "$request")"
request_timeout_ms="$(jq -er '.evaluation_policy.request_timeout_ms' "$request")"
takeover_margin="$(jq -er '.evaluation_policy.takeover_margin' "$request")"
jq -e --arg round_id "$round_id" --arg providers_sha256 "$providers_sha256" \
    --argjson replicates "$replicates" --argjson request_timeout_ms "$request_timeout_ms" \
    --argjson takeover_margin "$takeover_margin" \
    '.score_schema == 1 and .kind == "sim-latency-score-baseline" and
     .scorer_version == "sim-latency-score/1" and .round_id == $round_id and
     .config_sha256 == $providers_sha256 and .request_timeout_ms == $request_timeout_ms and
     .takeover_margin == $takeover_margin and (.replicates | length) == $replicates' \
    "$attempt_directory/baseline.json" >/dev/null ||
    die "rebaseline baseline identity or policy mismatch"

readonly score_gate_keys='["G1_success","G2_volume","G3_path_integrity","G4_matchmaking","G5_stability","G6_resources"]'
jq -e --arg job_id "$job_id" --argjson score_gate_keys "$score_gate_keys" \
    '.schema == 1 and .job_id == $job_id and .eval_error == null and
     .score.score_schema == 1 and .score.placeable == true and
     ((.score.gates | keys | sort) == $score_gate_keys) and
     ([.score.gates[] | .passed == true] | all)' \
    "$attempt_directory/worker-result.json" >/dev/null ||
    die "rebaseline score does not contain six passing frozen gates"

result_sha256="$(sha256_file "$result")"
containment="$output_directory/containment-promotion.json"
"$PROMOTE_CONTAINMENT" \
    --host-config "$host_config" \
    --evaluation-dir "$attempt_directory" \
    --resource-bomb-report "$resource_bomb_report" > "$containment.new"
chmod 0400 "$containment.new"
mv "$containment.new" "$containment"
containment_sha256="$(sha256_file "$containment")"
jq -e --arg job_id "$job_id" --arg round_id "$round_id" \
    '.schema == 1 and .kind == "sim-latency-host-containment-promotion" and
     .promoted == true and .job_id == $job_id and .round_id == $round_id' \
    "$containment" >/dev/null || die "containment promotion identity mismatch"

marker="$output_directory/rebaseline.json"
jq -n --arg promoted_at "$(date -u '+%FT%TZ')" \
    --arg round_id "$round_id" --arg job_id "$job_id" \
    --arg image "$(jq -er '.evaluator_image_digest' "$result")" \
    --arg baseline_sha "$(jq -er '.baseline_sha256' "$result")" \
    --arg manifest_sha "$(jq -er '.evidence_manifest_sha256' "$result")" \
    --arg result_sha "$result_sha256" --arg containment_sha "$containment_sha256" \
    '{schema:1,kind:"sim-latency-round-rebaseline",promoted_at:$promoted_at,
      round_id:$round_id,job_id:$job_id,image_digest:$image,
      baseline_sha256:$baseline_sha,evidence_manifest_sha256:$manifest_sha,
      rebaseline_evaluation_sha256:$result_sha,
      containment_promotion_sha256:$containment_sha,passed:true}' > "$marker.new"
chmod 0400 "$marker.new"
mv "$marker.new" "$marker"
marker_sha256="$(sha256_file "$marker")"

marker_path="$(jq -er '.rebaseline_manifest' "$host_config")"
[ "$(realpath -m -- "$marker_path")" = "$marker_path" ] && [ "$marker_path" != / ] ||
    die "configured rebaseline marker path is unsafe"
marker_parent="$(dirname -- "$marker_path")"
[ -d "$marker_parent" ] && [ ! -L "$marker_parent" ] &&
    [ "$(stat -c %u "$marker_parent")" -eq 0 ] || die "marker parent is unsafe"
marker_parent_mode="$(stat -c %a "$marker_parent")"
[ $((8#$marker_parent_mode & 0022)) -eq 0 ] || die "marker parent is group/world writable"
host_parent="$(dirname -- "$host_config")"
host_pending="$host_parent/.competition-host-rebaseline.$$.new"
marker_pending="$marker_parent/.competition-rebaseline.$$.new"
trap 'rm -f -- "$host_pending" "$marker_pending"' EXIT
install -o 0 -g 0 -m 0600 "$marker" "$marker_pending"
mv -f "$marker_pending" "$marker_path"
jq --arg marker_sha "$marker_sha256" \
    '.rebaseline_manifest_sha256 = $marker_sha' "$host_config" > "$host_pending"
chmod 0600 "$host_pending"
sync -d "$host_pending" "$marker_path"
mv -f "$host_pending" "$host_config"
sync -d "$host_config"
sync "$host_parent" "$marker_parent"

host_copy="$output_directory/competition-host.rebaseline.json"
install -o 0 -g 0 -m 0400 "$host_config" "$host_copy"
host_report="$output_directory/host-self-check.json"
management_cpus="$(jq -er '.management_cpu_list' "$host_config")"
taskset -c "$management_cpus" "$self_check" --json > "$host_report.new"
chmod 0400 "$host_report.new"
mv "$host_report.new" "$host_report"
jq -e --arg round_id "$round_id" \
    --arg image "$(jq -er '.image_digest' "$host_config")" \
    --arg hardware "$(jq -er '.hardware_id' "$host_config")" \
    --arg qualification "$(jq -er '.qualification_sha256' "$host_config")" \
    '.schema == 1 and .image_digest == $image and .hardware_id == $hardware and
     .qualification_sha256 == $qualification and
     .rebaseline_passed == true and
     .rebaseline_round_id == $round_id and
     .cleanup_verified == true and .resource_limits_verified == true and
     .resource_bomb_cleanup_verified == true and
     .default_deny_network == true and .no_production_secrets == true and
     .management_cpu_reserved == true and .management_memory_reserved == true and
     ([.checks[]] | all)' "$host_report" >/dev/null ||
    die "post-promotion host self-check failed"

sync -d "$containment" "$marker" "$host_copy" "$host_report"
sync "$output_directory"
jq -cn --arg round_id "$round_id" --arg job_id "$job_id" \
    --arg marker_sha256 "$marker_sha256" \
    --arg host_report_sha256 "$(sha256_file "$host_report")" \
    '{schema:1,kind:"sim-latency-round-rebaseline-promotion",
      round_id:$round_id,job_id:$job_id,marker_sha256:$marker_sha256,
      host_report_sha256:$host_report_sha256,promoted:true}'
