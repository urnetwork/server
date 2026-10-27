#!/usr/bin/env bash

# A separate v4 worker role reuses only the exact sealed v3 control producer.
# Install this file with unchanged v3 dependencies in a NEW protected directory.
# It cannot run build_candidate, candidate code, or publish a submission result.
set -Eeuo pipefail
umask 077

baseline_request=""
baseline_receipt=""
baseline_owner=""
baseline_context_sha256=""
while [ "$#" -gt 0 ]; do
    [ "$#" -ge 2 ] || exit 2
    case "$1" in
        --request) baseline_request="$2" ;;
        --receipt) baseline_receipt="$2" ;;
        --owner) baseline_owner="$2" ;;
        --context-sha256) baseline_context_sha256="$2" ;;
        *) exit 2 ;;
    esac
    shift 2
done
[[ "$baseline_owner" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$ && "$baseline_context_sha256" =~ ^[0-9a-f]{64}$ ]] || exit 2
[[ "$baseline_request" = /* && "$baseline_receipt" = /* ]] || exit 2
baseline_release="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
baseline_artifacts="$(dirname -- "$baseline_request")"
[ "$baseline_receipt" = "$baseline_artifacts/round-baseline-preparation.complete.json" ] || exit 2
[ ! -e "$baseline_receipt" ] && [ ! -L "$baseline_receipt" ] || exit 2
jq -e '.evaluation_policy.replicates == 9 and .round_baseline_path == "" and .round_baseline_sha256 == ""' "$baseline_request" >/dev/null

# round_baseline_prefix_program_begin
# No configurable source transformation: the complete source SHA and unique
# literal boundary identify exactly the qualified v3 baseline implementation.
round_baseline_control_prefix() {
    local source="$1" program prefix count
    [ -f "$source" ] && [ ! -L "$source" ] || return 1
    [ "$(sha256sum -- "$source" | awk '{print $1}')" = 0cccda1d1dcc227f30fda800ddb9bbb28e29dd48cc62885da637fcd4eb4eb00b ] || return 1
    program="$(<"$source")"
    count="$(printf '%s\n' "$program" | awk '$0 == "build_candidate" {n++} END {print n+0}')"
    [ "$count" = 1 ] || return 1
    prefix="${program%%$'\nbuild_candidate\n'*}"
    [ "$prefix" != "$program" ] || return 1
    printf '%s\n' "$prefix"
}
# round_baseline_prefix_program_end

baseline_prefix="$(round_baseline_control_prefix "$baseline_release/evaluator.sh")"
# v3 validates this reserved legacy output spelling, but its prefix never writes
# it. The only published result below has the separate baseline-purpose kind.
set -- --request "$baseline_request" --result "$baseline_artifacts/worker-result.json"
eval "$baseline_prefix"
unset baseline_prefix

# round_baseline_completion_program_begin
[ "$replicates" = 9 ] && [ "${#baseline_manifests[@]}" = 9 ] && [ "${#candidate_manifests[@]}" = 0 ] || die 'baseline-only control count mismatch'
[ ! -e "$artifact_dir/worker-result.json" ] && [ ! -e "$artifact_dir/evaluation.complete.json" ] || die 'baseline preparation gained submission-result authority'
[ -z "$(sudo -n docker ps -aq --filter "label=com.urnetwork.competition.job-id=$job_id")" ] || die 'baseline preparation left containers'
[ -z "$(sudo -n docker network ls -q --filter "label=com.urnetwork.competition.job-id=$job_id")" ] || die 'baseline preparation left networks'
remove_evaluation_sources
# Original per-stage raw output is retained once. These fixed tmpfs paths hold
# only hidden input/throwaway credentials or duplicate scorer copies.
[ "$work_dir" = "$artifact_dir/.evidence-runtime" ] && mountpoint -q "$work_dir" || die 'baseline preparation lost its owned tmpfs'
sudo -n rm -rf -- "$work_dir/input" "$work_dir/scorer-input" "$work_dir/score-runtime"
rm -f -- "$score_runner_env" "$score_compose_env"
validate_output_tree "$work_dir"
write_evaluation_progress baseline_prepared 9 0
persist_evidence_tree
write_evidence_manifest
[ -z "${active_work_mount:-}" ] || die 'baseline preparation evidence mount survived'
baseline_ids=()
for ordinal in 01 02 03 04 05 06 07 08 09; do
    baseline_ids+=("baseline-$ordinal-${job_id:0:8}")
done
baseline_ids_json="$(printf '%s\n' "${baseline_ids[@]}" | jq -Rsc 'split("\n")[:-1]')"
baseline_pending="$artifact_dir/.round-baseline-preparation.complete.json.new"
[ ! -e "$baseline_pending" ] && [ ! -L "$baseline_pending" ] || die 'baseline completion path already exists'
jq -n --arg owner "$baseline_owner" --arg context_sha256 "$baseline_context_sha256" \
    --arg job_id "$job_id" --arg round_id "$round_id" --arg base_sha "$base_sha" \
    --arg image "$base_image_id" --arg providers_sha256 "$providers_sha256" --arg scorer_version "$scorer_version" \
    --argjson source_epoch "$source_epoch" --argjson control_ids "$baseline_ids_json" \
    --arg baseline_sha256 "$baseline_sha256" --arg evidence_sha256 "$(sha256_file "$evidence_manifest")" \
    '{schema:1,kind:"sim-latency-round-baseline-preparation-complete",owner:$owner,context_sha256:$context_sha256,
      job_id:$job_id,round_id:$round_id,source_epoch:$source_epoch,base_sha:$base_sha,base_image_id:$image,
      providers_sha256:$providers_sha256,scorer_version:$scorer_version,replicates:9,control_evaluation_ids:$control_ids,
      baseline_sha256:$baseline_sha256,evidence_manifest_sha256:$evidence_sha256,
      submission_started:false,candidate_executed:false,cleanup_complete:true}' > "$baseline_pending"
chmod 0400 "$baseline_pending"
sync -d "$baseline_pending" "$artifact_dir/baseline.json" "$evidence_manifest"
[ ! -e "$baseline_receipt" ] && [ ! -L "$baseline_receipt" ] || die 'baseline completion appeared during publication'
mv -- "$baseline_pending" "$baseline_receipt"
sync "$artifact_dir"
log 'round baseline prepared; submission clock not started; no candidate or score executed'
# The inherited v3 EXIT trap still joins owned cleanup; it is never disabled.
# round_baseline_completion_program_end
