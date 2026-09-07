#!/usr/bin/env bash

# Exercise the evaluator's EXIT trap without running a workload. A deliberately
# mismatched scorer digest fails immediately after the evidence tmpfs is mounted,
# so this tests sanitizer invocation, unmount, object cleanup, and retention.

set -Eeuo pipefail
umask 077

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
[ "$#" -eq 1 ] || {
    printf 'usage: test-evaluator-failure-retention.sh REQUEST_TEMPLATE\n' >&2
    exit 2
}
template="$(realpath -e "$1")"
template_patch="$(jq -er '.patch_path' "$template")"
[ -f "$template_patch" ] || {
    printf 'template patch is missing\n' >&2
    exit 1
}

test_root="$(mktemp -d "${TMPDIR:-/tmp}/urnetwork-evaluator-failure.XXXXXXXX")"
request="$test_root/worker-request.json"
result="$test_root/worker-result.json"
patch="$test_root/canonical.patch"
log="$test_root/evaluator.log"
job_id="$(cat /proc/sys/kernel/random/uuid)"

cleanup() {
    while IFS= read -r container_id; do
        [ -n "$container_id" ] && sudo -n docker rm -f "$container_id" >/dev/null 2>&1 || true
    done < <(sudo -n docker ps -aq --filter "label=com.urnetwork.competition.job-id=$job_id" 2>/dev/null || true)
    while IFS= read -r network_id; do
        [ -n "$network_id" ] && sudo -n docker network rm "$network_id" >/dev/null 2>&1 || true
    done < <(sudo -n docker network ls -q --filter "label=com.urnetwork.competition.job-id=$job_id" 2>/dev/null || true)
    if mountpoint -q "$test_root/.evidence-runtime"; then
        sudo -n umount "$test_root/.evidence-runtime" >/dev/null 2>&1 || true
    fi
    if [ "${FAILURE_RETENTION_KEEP_ARTIFACTS:-no}" = yes ]; then
        chmod -R u+rX "$test_root" >/dev/null 2>&1 || true
        printf 'retained evaluator failure fixture: %s\n' "$test_root" >&2
        return
    fi
    chmod -R u+w "$test_root" >/dev/null 2>&1 || true
    rm -rf -- "$test_root"
}
trap cleanup EXIT INT TERM

install -m 0400 "$template_patch" "$patch"
jq \
    --arg artifact_directory "$test_root" \
    --arg job_id "$job_id" \
    --arg patch_path "$patch" \
    '.artifact_directory = $artifact_directory |
     .attempt = 97 |
     .job_id = $job_id |
     .patch_path = $patch_path |
     .evaluation_policy.scorer_sha256 = "0000000000000000000000000000000000000000000000000000000000000000"' \
    "$template" > "$request"
chmod 0400 "$request"

set +e
env LANG=en_US.UTF-8 LC_ALL=en_US.UTF-8 \
    "$SCRIPT_DIR/evaluator.sh" --request "$request" --result "$result" > "$log" 2>&1
status=$?
set -e
[ "$status" -ne 0 ] || {
    printf 'evaluator unexpectedly accepted the mismatched scorer digest\n' >&2
    exit 1
}

test ! -e "$result"
test ! -e "$test_root/.evidence-runtime"
test -s "$test_root/failed-evidence/failure.json"
test -s "$test_root/failed-evidence/resource-boundary.json"
test -s "$test_root/failed-evidence/evidence-quota.json"
test -s "$test_root/failed-evidence-manifest.json"
test ! -e "$test_root/failed-evidence/input"
test ! -e "$test_root/failed-evidence/scorer-input"
[ -z "$(find "$test_root/failed-evidence" -type d -name runtime -print -quit)" ]
[ -z "$(find "$test_root/failed-evidence" -type f \( -name '*.env' -o -name '*.env.new' \) -print -quit)" ]
[ -z "$(find "$test_root/failed-evidence" -mindepth 1 ! -type f ! -type d -print -quit)" ]
jq -e --arg job_id "$job_id" '
    .kind == "sim-latency-evaluator-failure" and .job_id == $job_id and
    .attempt == 97 and .exit_code > 0 and .evaluator_line > 0 and
    .sanitized == true
' "$test_root/failed-evidence/failure.json" >/dev/null
jq -e --arg job_id "$job_id" '
    .kind == "sim-latency-failed-evidence-manifest" and .job_id == $job_id and
    .attempt == 97 and .sanitized == true and (.artifacts | length) >= 3
' "$test_root/failed-evidence-manifest.json" >/dev/null
rg -q 'frozen scorer digest does not match pristine image' "$log"
rg -q 'retained sanitized failure evidence' "$log"
[ -z "$(sudo -n docker ps -aq --filter "label=com.urnetwork.competition.job-id=$job_id")" ]
[ -z "$(sudo -n docker network ls -q --filter "label=com.urnetwork.competition.job-id=$job_id")" ]

jq -n --argjson evaluator_exit_code "$status" \
    '{schema:1,passed:true,evaluator_exit_code:$evaluator_exit_code,checks:[
      "intentional post-mount infrastructure failure",
      "caller-locale-independent local mount authentication",
      "sanitized diagnostic retention",
      "hidden input removal",
      "credential path removal",
      "tmpfs unmounted",
      "zero residual containers",
      "zero residual networks"
    ]}'
