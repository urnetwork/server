#!/usr/bin/env bash

# Consume the completed attempt-06 production staging evidence and perform the
# one-way readiness seal, report render, and fail-closed completion audit. The
# release and 9+9 staging campaign are deliberately not repeated here.

set -Eeuo pipefail
umask 077
export LANG=C LC_ALL=C TZ=UTC

readonly ROOT=/home/by/urnetwork/server-finalization-evidence/connect/sim-latency/eval-12c/final-calibration-p1800-cf0fd3a9
readonly HISTORICAL_ROOT=/home/by/urnetwork/server/connect/sim-latency/eval-12c/final-calibration-p1800-cf0fd3a9
readonly SERVER=/home/by/urnetwork/server
readonly HANDOFF="$ROOT/finalization-handoff-attempt-06"
readonly STATUS="$HANDOFF/status.json"
readonly SEALER="$ROOT/seal-production-readiness.py"
readonly RENDERER="$ROOT/render-finalization-artifacts.py"
readonly AUDITOR="$ROOT/audit-finalization.py"
readonly SOURCE_LOCK="$ROOT/source-lock.json"
readonly HISTORICAL_SOURCE_LOCK="$HISTORICAL_ROOT/source-lock.json"
readonly REMEDIATION="$ROOT/production-staging-attempt-06-remediation-amendment.json"
readonly EVIDENCE_BINDING_AMENDMENT="$ROOT/production-staging-attempt-06-evidence-binding-amendment.json"
readonly READINESS="$ROOT/production-readiness-final.json"
readonly REPORT="$SERVER/finalize-report.html"
readonly PREVIEW="$SERVER/final-preview.html"
readonly REPORT_EVIDENCE="$ROOT/finalize-report-evidence.json"
readonly CALIBRATION_DOCUMENT="$SERVER/connect/sim-latency/APEX-CALIBRATION.md"
readonly FINAL_AUDIT="$HANDOFF/finalization-audit.json"
readonly SOURCE_LOCK_SHA=94c25024a92b5fcb5fa8bf324ff8022fde1074fd62bc210fc0ad5efbba0e4022
readonly HISTORICAL_SOURCE_LOCK_SHA=0cf71458833f3b1ae96a663357c583eba3a9c25a19d6c795c8549e4154141838
readonly REMEDIATION_SHA=7971eeeac22c73781c0de1ce34c5296f79b2f223afbfe67d4a7b3fd2642de65d
readonly EVIDENCE_BINDING_AMENDMENT_SHA=40ecb634563fa58fc41e346efdba6b604b2b86c7cb4fea820cc893e363191752
readonly SEALER_SHA=954f499e90ac795633f140c015da54185f6af5502feef62dfb547d79e4d3d656
readonly RENDERER_SHA=668dcfa4912254f5049df6acb55287cdb8b573e0fb7a7eac37ca896d448163ca
readonly AUDITOR_SHA=2e5de349246914029641548b04ee1ad7e488f72534751319bbaa6e69a69cbc21
readonly BOOT_ID=34760d1b-a0b6-46a0-b8c1-264abd1affba

readonly -a STAGING_CHECKS=(
    "$ROOT/production-readiness/release-artifacts.json"
    "$ROOT/production-readiness/service-backed-fifo-cache-failover.json"
    "$ROOT/production-readiness/authenticated-api.json"
    "$ROOT/production-readiness/full-staging-round.json"
    "$ROOT/production-readiness/monitoring-and-recovery.json"
    "$ROOT/production-readiness/artifact-retention.json"
    "$ROOT/production-readiness/no-secrets-audit.json"
)

stage=initializing
complete=false

log() { printf '[finalization-handoff] %s %s\n' "$(date -u '+%FT%TZ')" "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }
sha256_file() { sha256sum "$1" | awk '{print $1}'; }
require_command() { command -v "$1" >/dev/null 2>&1 || die "missing command: $1"; }

write_terminal_status() {
    local state="$1" rc="$2" pending="$STATUS.new"
    [ ! -e "$STATUS" ] || return 0
    jq -n \
        --arg state "$state" \
        --arg stage "$stage" \
        --arg recorded_at "$(date -u '+%FT%TZ')" \
        --argjson exit_code "$rc" \
        --arg source_lock_sha256 "$SOURCE_LOCK_SHA" \
        --arg historical_source_lock_sha256 "$HISTORICAL_SOURCE_LOCK_SHA" \
        --arg remediation_sha256 "$REMEDIATION_SHA" \
        --arg evidence_binding_amendment_sha256 "$EVIDENCE_BINDING_AMENDMENT_SHA" \
        --arg boot_id "$BOOT_ID" \
        --arg readiness_sha256 "$([ -f "$READINESS" ] && sha256_file "$READINESS" || printf '')" \
        --arg report_sha256 "$([ -f "$REPORT" ] && sha256_file "$REPORT" || printf '')" \
        --arg preview_sha256 "$([ -f "$PREVIEW" ] && sha256_file "$PREVIEW" || printf '')" \
        --arg report_evidence_sha256 "$([ -f "$REPORT_EVIDENCE" ] && sha256_file "$REPORT_EVIDENCE" || printf '')" \
        --arg calibration_document_sha256 "$([ -f "$CALIBRATION_DOCUMENT" ] && sha256_file "$CALIBRATION_DOCUMENT" || printf '')" \
        --arg final_audit_sha256 "$([ -f "$FINAL_AUDIT" ] && sha256_file "$FINAL_AUDIT" || printf '')" \
        '{schema:1,kind:"sim-latency-finalization-handoff",state:$state,
          stage:$stage,recorded_at:$recorded_at,exit_code:$exit_code,
          source_lock_sha256:$source_lock_sha256,
          historical_calibration_source_lock_sha256:$historical_source_lock_sha256,
          production_staging_attempt_06_remediation_amendment_sha256:$remediation_sha256,
          production_staging_attempt_06_evidence_binding_amendment_sha256:$evidence_binding_amendment_sha256,
          boot_id:$boot_id,evidence_sha256:{production_readiness:$readiness_sha256,
            final_report:$report_sha256,final_preview:$preview_sha256,
            final_report_evidence:$report_evidence_sha256,
            calibration_document:$calibration_document_sha256,
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

verify_dependencies() {
    while read -r path expected; do
        [ -f "$path" ] || die "handoff dependency is missing: $path"
        [ "$(sha256_file "$path")" = "$expected" ] ||
            die "handoff dependency changed: $path"
    done <<EOF
$SOURCE_LOCK $SOURCE_LOCK_SHA
$HISTORICAL_SOURCE_LOCK $HISTORICAL_SOURCE_LOCK_SHA
$REMEDIATION $REMEDIATION_SHA
$EVIDENCE_BINDING_AMENDMENT $EVIDENCE_BINDING_AMENDMENT_SHA
$SEALER $SEALER_SHA
$RENDERER $RENDERER_SHA
$AUDITOR $AUDITOR_SHA
EOF
}

for command in awk chmod date find id install jq mv sha256sum stat; do
    require_command "$command"
done
[ "$(id -u)" -eq 0 ] || die "finalization handoff must run as root"
[ "$#" -eq 0 ] || die "usage: $0"
[ ! -e "$HANDOFF" ] || die "handoff directory already exists: $HANDOFF"
install -d -o 0 -g 0 -m 0700 "$HANDOFF"
[ "$(< /proc/sys/kernel/random/boot_id)" = "$BOOT_ID" ] || die "host rebooted"
verify_dependencies

stage=production_staging_evidence
for path in "${STAGING_CHECKS[@]}"; do
    [ -f "$path" ] || die "production staging evidence is missing: $path"
    [ "$(stat -c '%U:%G:%a' "$path")" = root:root:400 ] ||
        die "production staging evidence is not sealed root:root 0400: $path"
done
[ ! -e "$READINESS" ] || die "final readiness already exists: $READINESS"
[ ! -e "$REPORT" ] || die "final report already exists: $REPORT"
[ ! -e "$REPORT_EVIDENCE" ] || die "report evidence already exists: $REPORT_EVIDENCE"

stage=readiness_seal
verify_dependencies
log "sealing attempt-06 production readiness"
"$SEALER" >"$HANDOFF/readiness-seal.log" 2>&1

stage=final_artifacts
verify_dependencies
log "validating and rendering calibration, final report, and preview"
"$RENDERER" --preflight >"$HANDOFF/final-artifacts-preflight.log" 2>&1
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
