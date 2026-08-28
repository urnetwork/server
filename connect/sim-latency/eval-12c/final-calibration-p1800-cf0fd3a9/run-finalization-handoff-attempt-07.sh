#!/usr/bin/env bash

# Preserve the failed attempt-06 handoff, repair its evidence-only audit
# predicate, add significant-improvement fringes to every threshold visual,
# and produce a new fail-closed completion audit.

set -Eeuo pipefail
umask 077
export LANG=C LC_ALL=C TZ=UTC

readonly ROOT=/home/by/urnetwork/server-finalization-evidence/connect/sim-latency/eval-12c/final-calibration-p1800-cf0fd3a9
readonly SERVER=/home/by/urnetwork/server
readonly EVIDENCE_SERVER=/home/by/urnetwork/server-finalization-evidence
readonly PRIOR_HANDOFF="$ROOT/finalization-handoff-attempt-06"
readonly HANDOFF="$ROOT/finalization-handoff-attempt-07"
readonly STATUS="$HANDOFF/status.json"
readonly PRIOR_STATUS="$PRIOR_HANDOFF/status.json"
readonly PRIOR_AUDIT="$PRIOR_HANDOFF/finalization-audit.json.new"
readonly REMEDIATION="$ROOT/finalization-handoff-attempt-07-remediation.json"
readonly AUDITOR="$ROOT/audit-finalization.py"
readonly RENDERER="$ROOT/render-finalization-artifacts.py"
readonly READINESS="$ROOT/production-readiness-final.json"
readonly REPORT="$SERVER/finalize-report.html"
readonly EVIDENCE_REPORT="$EVIDENCE_SERVER/finalize-report.html"
readonly PREVIEW="$SERVER/final-preview.html"
readonly EVIDENCE_PREVIEW="$EVIDENCE_SERVER/final-preview.html"
readonly CALIBRATION="$SERVER/connect/sim-latency/APEX-CALIBRATION.md"
readonly EVIDENCE_CALIBRATION="$EVIDENCE_SERVER/connect/sim-latency/APEX-CALIBRATION.md"
readonly REPORT_EVIDENCE="$ROOT/finalize-report-evidence.json"
readonly FINAL_AUDIT="$HANDOFF/finalization-audit.json"
readonly BOOT_ID=34760d1b-a0b6-46a0-b8c1-264abd1affba
readonly PRIOR_STATUS_SHA=a826b65299b61b67595e06c42b3be99452cab45568673a5207ea1fd3726f0ffc
readonly PRIOR_AUDIT_SHA=a2d2fc0673b9e275a7cc112a238b5119ca5ea1a5cacc0ff2dbba5904f5b519f7
readonly REMEDIATION_SHA=18c2bbbfcddb7b7540ced09dbcd61d1283a4264e6407b34391eccb8a6666cafa
readonly AUDITOR_SHA=e8f1c530b499ca79f9d17d0686075952722b3e4f476f1ac9ed8aa512010a0a98
readonly RENDERER_SHA=0b4cf2968b4d2ac2333b1984beb05c32b53119af28e6b1c79959f21386006e1a
readonly READINESS_SHA=bc56f7b02a1cfcfb5cd91eca0e885bc6aedb2b216d336bbb7b1f1288cd7e2f2d
readonly PRIOR_REPORT_SHA=f77c75945cc1d9aa551a6c77fe8037a902abbbb47b8de5afea42459277c27e15
readonly PRIOR_PREVIEW_SHA=9d6426124adf86256e0de3921f6c390a5ffa2cc64ff1ca45248b21997b97724b
readonly PRIOR_CALIBRATION_SHA=103424b828aa6356701d844bb5e80ac60e351cf2b03763af0b09f0dc0c924936
readonly PRIOR_REPORT_EVIDENCE_SHA=33cf779d4d50a0404ed187bc49317ef2086b1e6461600b169481cec47e400c8d

stage=initializing
complete=false

log() { printf '[finalization-handoff-07] %s %s\n' "$(date -u '+%FT%TZ')" "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }
sha256_file() { sha256sum "$1" | awk '{print $1}'; }
require_command() { command -v "$1" >/dev/null 2>&1 || die "missing command: $1"; }
verify_hash() {
    local path="$1" expected="$2"
    [ -f "$path" ] || die "missing dependency: $path"
    [ ! -L "$path" ] || die "symlink dependency: $path"
    [ "$(sha256_file "$path")" = "$expected" ] || die "dependency changed: $path"
}

write_terminal_status() {
    local state="$1" rc="$2" pending="$STATUS.new"
    [ ! -e "$STATUS" ] || return 0
    jq -n \
        --arg state "$state" \
        --arg stage "$stage" \
        --arg recorded_at "$(date -u '+%FT%TZ')" \
        --argjson exit_code "$rc" \
        --arg boot_id "$BOOT_ID" \
        --arg script_sha256 "$(sha256_file "$0")" \
        --arg prior_status_sha256 "$PRIOR_STATUS_SHA" \
        --arg prior_audit_sha256 "$PRIOR_AUDIT_SHA" \
        --arg remediation_sha256 "$REMEDIATION_SHA" \
        --arg auditor_sha256 "$AUDITOR_SHA" \
        --arg renderer_sha256 "$RENDERER_SHA" \
        --arg readiness_sha256 "$([ -f "$READINESS" ] && sha256_file "$READINESS" || printf '')" \
        --arg report_sha256 "$([ -f "$REPORT" ] && sha256_file "$REPORT" || printf '')" \
        --arg preview_sha256 "$([ -f "$PREVIEW" ] && sha256_file "$PREVIEW" || printf '')" \
        --arg calibration_sha256 "$([ -f "$CALIBRATION" ] && sha256_file "$CALIBRATION" || printf '')" \
        --arg report_evidence_sha256 "$([ -f "$REPORT_EVIDENCE" ] && sha256_file "$REPORT_EVIDENCE" || printf '')" \
        --arg final_audit_sha256 "$([ -f "$FINAL_AUDIT" ] && sha256_file "$FINAL_AUDIT" || printf '')" \
        '{schema:1,kind:"sim-latency-finalization-handoff",attempt:7,
          supersedes_failed_attempt:6,state:$state,stage:$stage,
          recorded_at:$recorded_at,exit_code:$exit_code,boot_id:$boot_id,
          script_sha256:$script_sha256,prior_status_sha256:$prior_status_sha256,
          prior_audit_sha256:$prior_audit_sha256,
          remediation_sha256:$remediation_sha256,auditor_sha256:$auditor_sha256,
          renderer_sha256:$renderer_sha256,evidence_sha256:{
            production_readiness:$readiness_sha256,final_report:$report_sha256,
            final_preview:$preview_sha256,calibration_document:$calibration_sha256,
            final_report_evidence:$report_evidence_sha256,
            final_audit:$final_audit_sha256}}' >"$pending"
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

for command in awk chmod date find id install jq mv sha256sum stat; do
    require_command "$command"
done
[ "$(id -u)" -eq 0 ] || die "attempt-07 handoff must run as root"
[ "$#" -eq 0 ] || die "usage: $0"
[ ! -e "$HANDOFF" ] || die "handoff directory already exists: $HANDOFF"
[ "$(< /proc/sys/kernel/random/boot_id)" = "$BOOT_ID" ] || die "host rebooted"
install -d -o 0 -g 0 -m 0700 "$HANDOFF"

stage=prior_failure_authentication
verify_hash "$PRIOR_STATUS" "$PRIOR_STATUS_SHA"
verify_hash "$PRIOR_AUDIT" "$PRIOR_AUDIT_SHA"
verify_hash "$REMEDIATION" "$REMEDIATION_SHA"
verify_hash "$AUDITOR" "$AUDITOR_SHA"
verify_hash "$RENDERER" "$RENDERER_SHA"
verify_hash "$READINESS" "$READINESS_SHA"
verify_hash "$REPORT" "$PRIOR_REPORT_SHA"
verify_hash "$EVIDENCE_REPORT" "$PRIOR_REPORT_SHA"
verify_hash "$PREVIEW" "$PRIOR_PREVIEW_SHA"
verify_hash "$EVIDENCE_PREVIEW" "$PRIOR_PREVIEW_SHA"
verify_hash "$CALIBRATION" "$PRIOR_CALIBRATION_SHA"
verify_hash "$EVIDENCE_CALIBRATION" "$PRIOR_CALIBRATION_SHA"
verify_hash "$REPORT_EVIDENCE" "$PRIOR_REPORT_EVIDENCE_SHA"
[ "$(stat -c '%U:%G:%a' "$PRIOR_STATUS")" = root:root:400 ] || die "prior status is not sealed"
[ "$(stat -c '%U:%G:%a' "$PRIOR_AUDIT")" = root:root:400 ] || die "prior audit is not sealed"
[ "$(stat -c '%U:%G:%a' "$READINESS")" = root:root:400 ] || die "readiness is not sealed"
jq -e '.state == "failed" and .stage == "completion_audit" and .exit_code == 1' "$PRIOR_STATUS" >/dev/null || die "prior status is not the expected terminal failure"
jq -e '.local_finalization_complete == false and .required_passes == 9 and
    .required_pending == 0 and .required_failures == 1 and
    ([.checks[] | select(.launch_required and .state == "fail")] | length) == 1 and
    ([.checks[] | select(.launch_required and .state == "fail")][0].id) == "production_staging" and
    ([.checks[] | select(.launch_required and .state == "fail")][0].summary) ==
      "production-readiness evidence is not complete"' "$PRIOR_AUDIT" >/dev/null || die "prior audit failure changed"
jq -e '.authorized == true and .prior_handoff.measurement_effect == false and
    .correction.exact_value_authentication == true and
    .correction.threshold_line_label_and_fringe_mapping_required == true and
    .successor.required_passes == 10 and .successor.required_failures == 0' "$REMEDIATION" >/dev/null || die "remediation authorization is invalid"

stage=deterministic_regression
log "running corrected auditor and renderer self-tests"
"$AUDITOR" --self-test >"$HANDOFF/auditor-self-test.log" 2>&1
"$RENDERER" --self-test >"$HANDOFF/renderer-self-test.log" 2>&1
"$RENDERER" --preflight >"$HANDOFF/final-artifacts-preflight.log" 2>&1

stage=threshold_fringe_remediation
log "atomically adding significant-improvement fringes to final visuals"
"$RENDERER" --remediate-threshold-fringes >"$HANDOFF/final-artifacts-remediation.log" 2>&1
[ "$(sha256_file "$REPORT")" != "$PRIOR_REPORT_SHA" ] || die "report did not change"
[ "$(sha256_file "$PREVIEW")" != "$PRIOR_PREVIEW_SHA" ] || die "preview did not change"
[ "$(sha256_file "$REPORT")" = "$(sha256_file "$EVIDENCE_REPORT")" ] || die "report copies differ"
[ "$(sha256_file "$PREVIEW")" = "$(sha256_file "$EVIDENCE_PREVIEW")" ] || die "preview copies differ"
[ "$(sha256_file "$CALIBRATION")" = "$(sha256_file "$EVIDENCE_CALIBRATION")" ] || die "calibration copies differ"

stage=completion_audit
log "running corrected fail-closed completion audit"
"$AUDITOR" --require-complete >"$FINAL_AUDIT.new"
jq -e '.local_finalization_complete == true and .required_passes == 10 and
    .required_pending == 0 and .required_failures == 0' "$FINAL_AUDIT.new" >/dev/null || die "completion audit did not report 10/10 required passes"
chmod 0400 "$FINAL_AUDIT.new"
mv "$FINAL_AUDIT.new" "$FINAL_AUDIT"

stage=complete
write_terminal_status complete 0
complete=true
trap - EXIT INT TERM
find "$HANDOFF" -maxdepth 1 -type f -name '*.log' -exec chmod 0400 {} +
log "attempt-07 local finalization completed"
