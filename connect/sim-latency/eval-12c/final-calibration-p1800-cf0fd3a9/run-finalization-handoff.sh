#!/usr/bin/env bash

# Wait for the exclusive calibration campaign, then execute the remaining
# release, staging, sealing, rendering, and completion-audit gates without an
# unattended gap. Failure evidence is retained and no later gate is attempted.

set -Eeuo pipefail
umask 077
export LANG=C LC_ALL=C TZ=UTC

readonly ROOT=/home/by/urnetwork/server/connect/sim-latency/eval-12c/final-calibration-p1800-cf0fd3a9
readonly SERVER=/home/by/urnetwork/server
readonly HANDOFF="$ROOT/finalization-handoff-attempt-04"
readonly STATUS="$HANDOFF/status.json"
readonly BUILDER="$ROOT/build-control-plane-release.sh"
readonly SERVICE_GATE="$ROOT/run-service-backed-release-gate.sh"
readonly STAGING="$ROOT/run-production-staging.sh"
readonly SEALER="$ROOT/seal-production-readiness.py"
readonly RENDERER="$ROOT/render-finalization-artifacts.py"
readonly AUDITOR="$ROOT/audit-finalization.py"
readonly IMPLEMENTATION="$ROOT/production-staging-implementation.json"
readonly SAME_PROGRESS="$ROOT/post-frontier/p1800-c200-r80-q2/same-seed/progress.json"
readonly SAME_ANALYSIS="$ROOT/post-frontier/p1800-c200-r80-q2/same-seed-analysis.json"
readonly PLACEABILITY_POLICY="$ROOT/launch-readiness-placeability-policy-amendment.json"
readonly POSTPROCESSING_REPAIR="$ROOT/same-seed-postprocessing-repair.json"
readonly REFERENCE_V5="$ROOT/reference-requalification-v5"
readonly INDEPENDENT_ATTESTATION="$REFERENCE_V5/hidden-launch-runtime/independent-campaign-attestation.json"
readonly INDEPENDENT_PROGRESS="$REFERENCE_V5/hidden-launch-runtime/independent-references/progress.json"
readonly INDEPENDENT_TERMINAL_DECISION="$REFERENCE_V5/hidden-launch-decision.json"
readonly INDEPENDENT_PROTOCOL="$REFERENCE_V5/hidden-launch-protocol.json"
readonly REFERENCE_V5_QUALIFICATION="$REFERENCE_V5/qualification.json"
readonly STAGING_REFERENCE_V5_AMENDMENT="$ROOT/production-staging-reference-v5-amendment.json"
readonly READINESS="$ROOT/production-readiness-final.json"
readonly REPORT="$SERVER/finalize-report.html"
readonly REPORT_EVIDENCE="$ROOT/finalize-report-evidence.json"
readonly CALIBRATION_DOCUMENT="$SERVER/connect/sim-latency/APEX-CALIBRATION.md"
readonly FINAL_AUDIT="$HANDOFF/finalization-audit.json"
readonly SAME_SERVICE=urnetwork-final-calibration-recovery-8c7cfc98.service
readonly INDEPENDENT_SERVICE=urnetwork-reference-v5-hidden-a889248b-attempt-01.service
readonly SOURCE_LOCK_SHA=0cf71458833f3b1ae96a663357c583eba3a9c25a19d6c795c8549e4154141838
readonly BOOT_ID=34760d1b-a0b6-46a0-b8c1-264abd1affba
readonly BUILDER_SHA=07efff117edfd2d96fbab24bba5597241de51419952f58ee7d259bdf9bebec73
readonly SERVICE_GATE_SHA=4bbc9b5decf4313d0853daa179ff28f10559e3cc9adb825a41ef377d400711de
readonly STAGING_SHA=PENDING_STAGING_SHA256
readonly SEALER_SHA=PENDING_SEALER_SHA256
readonly RENDERER_SHA=PENDING_RENDERER_SHA256
readonly AUDITOR_SHA=PENDING_AUDITOR_SHA256
readonly IMPLEMENTATION_SHA=80fd5573af4740f0be7509caa88378352306c920aaa6481f80cac7ee0db2eb9a
readonly SAME_PROGRESS_SHA=468fc8c9e3f1fd13a2d99e68ce1e680cc46a73e8c65c2ae7e9ad86a7121e0ffe
readonly SAME_ANALYSIS_SHA=41325e3aabd98495afb22264ab2981ad690e4e2ca7ebb295b40c40ea96d0de9c
readonly PLACEABILITY_POLICY_SHA=359fe89572c81d6602cbf0ece03e5128c5ccbf38bf2d20f22fcdbadd30f2f638
readonly POSTPROCESSING_REPAIR_SHA=62c7e8c1398d8619bf9bfb4645b0e7965a4e9a83713d47e0da86fc705ecd59dd
readonly INDEPENDENT_ATTESTATION_SHA=b96b216022b34e2bf0e9838ca51380431d9f97e95121610951c37d0274cc5c02
readonly INDEPENDENT_PROGRESS_SHA=f2bbe8797dc463bb85d576cea29575579757df3eb7109a2170fd0566008d1e8b
readonly INDEPENDENT_TERMINAL_DECISION_SHA=3e4cc70d783b01a87328736caf82f49016138c97ff384b26dc38864f8cede835
readonly INDEPENDENT_PROTOCOL_SHA=4969535eb343049d7b790c5fff8e82b7eb7a60b6e92d2e2aa94e6466e7789fad
readonly REFERENCE_V5_QUALIFICATION_SHA=8bdc86dcf68a8f8a4c686d8d6267510e121ab7800c9bbcc7cfa4dbce1ac1ca10
readonly STAGING_REFERENCE_V5_AMENDMENT_SHA=618393539636b69cfcdbd6fec14afef3e58fe20d43bda06fbcbf15693802b695

stage=initializing
complete=false

log() { printf '[finalization-handoff] %s %s\n' "$(date -u '+%FT%TZ')" "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }
sha256_file() { sha256sum "$1" | awk '{print $1}'; }
require_command() { command -v "$1" >/dev/null 2>&1 || die "missing command: $1"; }

write_terminal_status() {
    local state="$1" rc="$2"
    local pending="$STATUS.new"
    [ ! -e "$STATUS" ] || return 0
    jq -n \
        --arg state "$state" \
        --arg stage "$stage" \
        --arg recorded_at "$(date -u '+%FT%TZ')" \
        --argjson exit_code "$rc" \
        --arg source_lock_sha256 "$SOURCE_LOCK_SHA" \
        --arg boot_id "$BOOT_ID" \
        --arg implementation_sha256 "$IMPLEMENTATION_SHA" \
        --arg independent_attestation_sha256 "$([ -f "$INDEPENDENT_ATTESTATION" ] && sha256_file "$INDEPENDENT_ATTESTATION" || printf '')" \
        --arg independent_terminal_decision_sha256 "$([ -f "$INDEPENDENT_TERMINAL_DECISION" ] && sha256_file "$INDEPENDENT_TERMINAL_DECISION" || printf '')" \
        --arg independent_protocol_sha256 "$([ -f "$INDEPENDENT_PROTOCOL" ] && sha256_file "$INDEPENDENT_PROTOCOL" || printf '')" \
        --arg staging_reference_v5_amendment_sha256 "$([ -f "$STAGING_REFERENCE_V5_AMENDMENT" ] && sha256_file "$STAGING_REFERENCE_V5_AMENDMENT" || printf '')" \
        --arg readiness_sha256 "$([ -f "$READINESS" ] && sha256_file "$READINESS" || printf '')" \
        --arg report_sha256 "$([ -f "$REPORT" ] && sha256_file "$REPORT" || printf '')" \
        --arg report_evidence_sha256 "$([ -f "$REPORT_EVIDENCE" ] && sha256_file "$REPORT_EVIDENCE" || printf '')" \
        --arg calibration_document_sha256 "$([ -f "$CALIBRATION_DOCUMENT" ] && sha256_file "$CALIBRATION_DOCUMENT" || printf '')" \
        --arg final_audit_sha256 "$([ -f "$FINAL_AUDIT" ] && sha256_file "$FINAL_AUDIT" || printf '')" \
        '{schema:1,kind:"sim-latency-finalization-handoff",state:$state,stage:$stage,
          recorded_at:$recorded_at,exit_code:$exit_code,source_lock_sha256:$source_lock_sha256,
          boot_id:$boot_id,implementation_sha256:$implementation_sha256,
          evidence_sha256:{independent_attestation:$independent_attestation_sha256,
            independent_terminal_decision:$independent_terminal_decision_sha256,
            independent_protocol:$independent_protocol_sha256,
            staging_reference_v5_amendment:$staging_reference_v5_amendment_sha256,
            production_readiness:$readiness_sha256,final_report:$report_sha256,
            final_report_evidence:$report_evidence_sha256,
            calibration_document:$calibration_document_sha256,final_audit:$final_audit_sha256}}' \
        >"$pending"
    chmod 0400 "$pending"
    mv "$pending" "$STATUS"
}

on_exit() {
    local rc=$?
    trap - EXIT INT TERM
    if [ "$complete" != true ]; then
        write_terminal_status failed "$rc" || true
    fi
    find "$HANDOFF" -maxdepth 1 -type f -name '*.log' -exec chmod 0400 {} + 2>/dev/null || true
    exit "$rc"
}
trap on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

wait_for_service_idle() {
    local service="$1" state result
    while :; do
        state="$(systemctl is-active "$service" 2>/dev/null || true)"
        case "$state" in
            active|activating|deactivating)
                log "waiting for $service ($state)"
                sleep 30
                ;;
            *) break ;;
        esac
    done
    result="$(systemctl show "$service" -p Result --value 2>/dev/null || true)"
    [ "$state" != failed ] ||
        die "$service ended state=$state result=${result:-unknown}"
    [ -z "$result" ] || [ "$result" = success ] ||
        die "$service ended state=$state result=$result"
}

verify_dependencies() {
    while read -r path expected; do
        [ "$(sha256_file "$path")" = "$expected" ] || die "handoff dependency changed: $path"
    done <<EOF
$BUILDER $BUILDER_SHA
$SERVICE_GATE $SERVICE_GATE_SHA
$STAGING $STAGING_SHA
$SEALER $SEALER_SHA
$RENDERER $RENDERER_SHA
$AUDITOR $AUDITOR_SHA
$IMPLEMENTATION $IMPLEMENTATION_SHA
$SAME_PROGRESS $SAME_PROGRESS_SHA
$SAME_ANALYSIS $SAME_ANALYSIS_SHA
$PLACEABILITY_POLICY $PLACEABILITY_POLICY_SHA
$POSTPROCESSING_REPAIR $POSTPROCESSING_REPAIR_SHA
$INDEPENDENT_ATTESTATION $INDEPENDENT_ATTESTATION_SHA
$INDEPENDENT_PROGRESS $INDEPENDENT_PROGRESS_SHA
$INDEPENDENT_TERMINAL_DECISION $INDEPENDENT_TERMINAL_DECISION_SHA
$INDEPENDENT_PROTOCOL $INDEPENDENT_PROTOCOL_SHA
$REFERENCE_V5_QUALIFICATION $REFERENCE_V5_QUALIFICATION_SHA
$STAGING_REFERENCE_V5_AMENDMENT $STAGING_REFERENCE_V5_AMENDMENT_SHA
EOF
}

for command in awk chmod date find id install jq mv readlink runuser sha256sum sleep systemctl; do
    require_command "$command"
done
[ "$(id -u)" -eq 0 ] || die "finalization handoff must run as root"
[ "$#" -eq 0 ] || die "usage: $0"
[ ! -e "$HANDOFF" ] || die "handoff directory already exists: $HANDOFF"
install -d -o 0 -g 0 -m 0700 "$HANDOFF"
[ "$(< /proc/sys/kernel/random/boot_id)" = "$BOOT_ID" ] || die "host rebooted"
[ "$(sha256_file "$ROOT/source-lock.json")" = "$SOURCE_LOCK_SHA" ] || die "source lock changed"
verify_dependencies

stage=exclusive_measurements
same_state="$(systemctl is-active "$SAME_SERVICE" 2>/dev/null || true)"
case "$same_state" in
    active|activating|deactivating)
        die "repaired same-seed service is unexpectedly active: $same_state"
        ;;
esac
jq -e '.complete == true and .completed_pairs == 12 and .target_pairs == 12 and
    (.results | length) == 12' "$SAME_PROGRESS" >/dev/null ||
    die "repaired same-seed progress is not terminal"
jq -e '.kind == "sim-latency-launch-compromise-same-seed-analysis" and
    .decision_ready == true and .replicate_count == 12 and
    .recommended_replicates == 9 and .recommended_takeover_margin == 0.161 and
    .progress_sha256 == "468fc8c9e3f1fd13a2d99e68ce1e680cc46a73e8c65c2ae7e9ad86a7121e0ffe"' \
    "$SAME_ANALYSIS" >/dev/null || die "repaired same-seed analysis is not terminal"
jq -e '.passed == true and .measurements_rerun == false and
    .measurements_censored == false and .strict_analysis_retained == true and
    .placeability_policy_amendment_sha256 ==
      "359fe89572c81d6602cbf0ece03e5128c5ccbf38bf2d20f22fcdbadd30f2f638" and
    .terminal_progress_sha256 ==
      "468fc8c9e3f1fd13a2d99e68ce1e680cc46a73e8c65c2ae7e9ad86a7121e0ffe" and
    .launch_analysis_sha256 ==
      "41325e3aabd98495afb22264ab2981ad690e4e2ca7ebb295b40c40ea96d0de9c"' \
    "$POSTPROCESSING_REPAIR" >/dev/null ||
    die "same-seed post-processing repair is not authenticated"
wait_for_service_idle "$INDEPENDENT_SERVICE"
jq -e '.accepted == true and .target_independent_seeds == 5 and
    .reference_required_passes == 4 and .reference_ordering_passes >= 4 and
    .ordering_metric == "candidate_raw_score_ms_over_designated_baseline_raw_score_ms" and
    .one_designated_same_round_baseline_per_seed == true' \
    "$INDEPENDENT_ATTESTATION" >/dev/null || die "independent attestation did not pass"
jq -e '.complete == true and .completed_independent_seeds == 5 and
    .target_independent_seeds == 5 and .designated_independent_baselines == 5 and
    .replicates_per_reference == 1 and .reference_ordering_passes >= 4 and
    .separability_passed == true' \
    "$INDEPENDENT_PROGRESS" >/dev/null || die "independent progress is not terminal"
jq -e '.accepted == true and .campaign_exit_code == 0 and
    .completed_independent_seeds == 5 and .reference_required_passes == 4 and
    .reference_ordering_passes >= 4 and
    .ordering_metric == "candidate_raw_score_ms_over_designated_baseline_raw_score_ms" and
    .cleanup.residual_competition_containers == 0 and
    .cleanup.residual_competition_networks == 0' \
    "$INDEPENDENT_TERMINAL_DECISION" >/dev/null ||
    die "independent terminal decision did not pass"
jq -e '.draft == false and .authorized == true and
    .target_independent_seeds == 5 and .reference_required_passes == 4 and
    .ordering_metric == "candidate_raw_score_ms_over_designated_baseline_raw_score_ms"' \
    "$INDEPENDENT_PROTOCOL" >/dev/null || die "independent protocol is not final"
jq -e '.draft == false and .accepted_for_hidden_five_seed_screen == true and
    .official_reference_set_accepted == false and .strict_ordering_passed == true and
    ([.checks[].passed] | all)' "$REFERENCE_V5_QUALIFICATION" >/dev/null ||
    die "reference-v5 qualification is not final"
jq -e '.draft == false and .authorized == true and
    .replacement_measurement_dependencies.same_seed_pairs == 12 and
    .replacement_measurement_dependencies.independent_seeds == 5 and
    .replacement_measurement_dependencies.required_reference_ordering_passes == 4 and
    ([.retained_invariants[]] | all)' "$STAGING_REFERENCE_V5_AMENDMENT" >/dev/null ||
    die "reference-v5 staging amendment is not final"

stage=parallel_release_gates
verify_dependencies
log "starting release build and service-backed gate in parallel"
runuser -u by -- "$BUILDER" >"$HANDOFF/release-build.log" 2>&1 &
builder_pid=$!
runuser -u by -- "$SERVICE_GATE" >"$HANDOFF/service-backed.log" 2>&1 &
service_pid=$!
builder_rc=0
service_rc=0
wait "$builder_pid" || builder_rc=$?
wait "$service_pid" || service_rc=$?
[ "$builder_rc" -eq 0 ] || die "release build failed with status $builder_rc"
[ "$service_rc" -eq 0 ] || die "service-backed gate failed with status $service_rc"

stage=production_staging
verify_dependencies
log "starting authenticated production staging round"
"$STAGING" >"$HANDOFF/production-staging.log" 2>&1

stage=readiness_seal
verify_dependencies
log "sealing production readiness"
"$SEALER" >"$HANDOFF/readiness-seal.log" 2>&1

stage=final_artifacts
verify_dependencies
log "rendering final calibration document and four-section report"
"$RENDERER" >"$HANDOFF/final-artifacts.log" 2>&1

stage=completion_audit
verify_dependencies
log "running fail-closed completion audit"
"$AUDITOR" --require-complete >"$FINAL_AUDIT.new"
jq -e '.local_finalization_complete == true and .required_passes == 10 and
    .required_pending == 0 and .required_failures == 0' "$FINAL_AUDIT.new" >/dev/null ||
    die "completion audit did not report 10/10 required passes"
chmod 0400 "$FINAL_AUDIT.new"
mv "$FINAL_AUDIT.new" "$FINAL_AUDIT"

stage=complete
write_terminal_status complete 0
complete=true
trap - EXIT INT TERM
find "$HANDOFF" -maxdepth 1 -type f -name '*.log' -exec chmod 0400 {} +
log "full local finalization completed"
