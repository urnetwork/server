#!/usr/bin/env bash

set -Eeuo pipefail
umask 077

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/urnetwork-failure-evidence.XXXXXXXX")"
source_dir="$test_root/.evidence-runtime"
destination_dir="$test_root/failed-evidence"

cleanup() {
    if mountpoint -q "$source_dir"; then
        sudo -n umount "$source_dir" >/dev/null 2>&1 || true
    fi
    chmod -R u+w "$test_root" >/dev/null 2>&1 || true
    rm -rf -- "$test_root"
}
trap cleanup EXIT INT TERM

install -d -m 0700 "$source_dir"
sudo -n mount -t tmpfs \
    -o "size=16m,mode=0700,uid=$(id -u),gid=$(id -g),nosuid,nodev,noexec" \
    urnetwork-failure-evidence-test "$source_dir"
install -d -m 0700 \
    "$source_dir/input" \
    "$source_dir/scorer-input" \
    "$source_dir/score-runtime" \
    "$source_dir/runs/baseline-01/runtime/config/local" \
    "$source_dir/runs/baseline-01/runtime/vault/local" \
    "$source_dir/runs/baseline-01/output/evaluation-1" \
    "$source_dir/runs/baseline-02"
printf '%s\n' hidden-provider-seed > "$source_dir/input/providers.yml"
printf '%s\n' hidden-provider-copy > "$source_dir/scorer-input/providers.yml"
printf '%s\n' throwaway-secret > "$source_dir/runs/baseline-01/runtime/vault/local/pg.yml"
printf '%s\n' throwaway-secret > "$source_dir/runs/baseline-01/runner.env"
printf '%s\n' throwaway-secret > "$source_dir/scorer.env.new"
printf '%s\n' '{"schema":2,"completion_state":"complete"}' \
    > "$source_dir/runs/baseline-01/output/evaluation-1/run.json"
printf '%s\n' '[sim-latency eval=evaluation-1] Unexpected error: retained diagnostic' \
    > "$source_dir/runs/baseline-01/output/evaluation-1/stderr.log"
printf '%s\n' 'baseline replicate 1 is not stable (findings: unexpected_recovery)' \
    > "$source_dir/baseline-scorer.log"
printf '%s\n' '[{"Config":{"Env":["EVALUATION_DB_PASSWORD=throwaway-secret"]},"Mounts":[{"Source":"/host/private"}]}]' \
    > "$source_dir/runs/baseline-01/containers.json"
printf '%s\n' '[{"id":"container-1","name":"runner","image_id":"sha256:test","config":{"image":"image","user":"65532:65532","labels":{}},"host_config":{},"mounts":[{"type":"bind","destination":"/runtime/config/local","rw":false}],"state":{}}]' \
    > "$source_dir/runs/baseline-02/containers.json"
ln -s /etc/passwd "$source_dir/runs/baseline-01/output/evaluation-1/host-link"
mkfifo "$source_dir/runs/baseline-01/output/evaluation-1/attacker-fifo"

env \
    FAILURE_JOB_ID=11111111-1111-1111-1111-111111111111 \
    FAILURE_ROUND_ID=22222222-2222-2222-2222-222222222222 \
    FAILURE_ATTEMPT=13 \
    FAILURE_EXIT_CODE=1 \
    FAILURE_EVALUATOR_LINE=1007 \
    "$SCRIPT_DIR/retain-failure-evidence.sh" "$source_dir" "$destination_dir"

test -s "$destination_dir/failure.json"
test -s "$destination_dir/baseline-scorer.log"
test -s "$destination_dir/runs/baseline-01/output/evaluation-1/run.json"
test -s "$destination_dir/runs/baseline-01/output/evaluation-1/stderr.log"
test -s "$test_root/failed-evidence-manifest.json"
test ! -e "$destination_dir/input"
test ! -e "$destination_dir/scorer-input"
test ! -e "$destination_dir/score-runtime"
test ! -e "$destination_dir/runs/baseline-01/runtime"
test ! -e "$destination_dir/runs/baseline-01/runner.env"
test ! -e "$destination_dir/runs/baseline-01/containers.json"
test -s "$destination_dir/runs/baseline-02/containers.json"
test ! -e "$destination_dir/scorer.env.new"
test ! -e "$destination_dir/runs/baseline-01/output/evaluation-1/host-link"
test ! -e "$destination_dir/runs/baseline-01/output/evaluation-1/attacker-fifo"
[ -z "$(find "$destination_dir" -mindepth 1 ! -type f ! -type d -print -quit)" ]
! rg -l 'throwaway-secret|hidden-provider' "$destination_dir" "$test_root/failed-evidence-manifest.json"
jq -e '
    .schema == 1 and .kind == "sim-latency-evaluator-failure" and
    .attempt == 13 and .exit_code == 1 and .evaluator_line == 1007 and
    .sanitized == true
' "$destination_dir/failure.json" >/dev/null
jq -e '
    .schema == 1 and .kind == "sim-latency-failed-evidence-manifest" and
    .attempt == 13 and .sanitized == true and
    ([.artifacts[].path] | index("failed-evidence/baseline-scorer.log") != null) and
    ([.artifacts[].path] | index("failed-evidence/runs/baseline-01/output/evaluation-1/stderr.log") != null)
' "$test_root/failed-evidence-manifest.json" >/dev/null
[ "$(stat -c '%a' "$destination_dir")" = 500 ]
[ "$(stat -c '%a' "$destination_dir/failure.json")" = 400 ]

jq -n '{schema:1,passed:true,checks:[
    "diagnostic artifacts retained",
    "runtime credentials removed",
    "hidden workload copies removed",
    "environment files removed",
    "non-regular attacker entries removed",
    "retained evidence sealed read-only",
    "failed-evidence manifest generated"
]}'
