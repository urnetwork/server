#!/usr/bin/env bash

# Copy a sanitized diagnostic tree out of the evaluator's bounded tmpfs after
# an infrastructure failure. This script intentionally mutates the source tree:
# per-stage credentials and hidden workload inputs must be gone before anything
# reaches durable retention.

set -Eeuo pipefail
umask 077

die() {
    printf '[retain-failure-evidence] ERROR: %s\n' "$*" >&2
    exit 1
}

for command in awk chmod cp dirname find install jq realpath rm sha256sum sort stat sudo sync; do
    command -v "$command" >/dev/null 2>&1 || die "missing command: $command"
done

[ "$#" -eq 2 ] || die 'usage: retain-failure-evidence.sh SOURCE_DIRECTORY DESTINATION_DIRECTORY'
source_dir="$1"
destination_dir="$2"
: "${FAILURE_JOB_ID:?}"
: "${FAILURE_ROUND_ID:?}"
: "${FAILURE_ATTEMPT:?}"
: "${FAILURE_EXIT_CODE:?}"
: "${FAILURE_EVALUATOR_LINE:?}"

[[ "$source_dir" = /* && "$destination_dir" = /* ]] || die 'paths must be absolute'
[ -d "$source_dir" ] && [ ! -L "$source_dir" ] || die 'source must be a non-symlink directory'
[ ! -e "$destination_dir" ] || die 'destination already exists'
source_dir="$(realpath -e "$source_dir")"
destination_parent="$(realpath -e "$(dirname "$destination_dir")")"
[ "$source_dir" = "$destination_parent/.evidence-runtime" ] || die 'source is not the evaluator evidence tmpfs'
[ "$destination_dir" = "$destination_parent/failed-evidence" ] || die 'destination name is not frozen'
[ "$(stat -f -c '%T' "$source_dir")" = tmpfs ] || die 'source evidence filesystem is not tmpfs'
[[ "$FAILURE_JOB_ID" =~ ^[0-9a-f-]{36}$ ]] || die 'job id is invalid'
[[ "$FAILURE_ROUND_ID" =~ ^[0-9a-f-]{36}$ ]] || die 'round id is invalid'
[[ "$FAILURE_ATTEMPT" =~ ^[1-9][0-9]*$ ]] || die 'attempt is invalid'
[[ "$FAILURE_EXIT_CODE" =~ ^[1-9][0-9]*$ ]] || die 'exit code is invalid'
[[ "$FAILURE_EVALUATOR_LINE" =~ ^[0-9]+$ ]] || die 'evaluator line is invalid'

# These paths contain either per-stage throwaway credentials or the hidden
# workload input. Completed run output remains available for diagnosis.
sudo -n rm -rf -- \
    "$source_dir/input" \
    "$source_dir/scorer-input" \
    "$source_dir/score-runtime"
sudo -n find "$source_dir" -mindepth 1 -maxdepth 1 -type d \
    -name 'evaluation-sources.*' -exec rm -rf -- {} +
sudo -n find "$source_dir" -depth -type d -name runtime -exec rm -rf -- {} +
sudo -n find "$source_dir" -type f \
    \( -name '*.env' -o -name '*.env.new' \) -delete
# A raw Docker inspection contains Config.Env and bind source paths. Current
# evaluators sanitize it in memory, but reject any legacy/partial file rather
# than trusting its filename during failure recovery.
while IFS= read -r -d '' inspection; do
    if ! sudo -n jq -e '
        type == "array" and all(.[];
          (keys | sort) == ["config","host_config","id","image_id","mounts","name","state"] and
          (.config | keys | sort) == ["image","labels","user"] and
          (.mounts | type == "array" and all(.[];
            (keys | sort) == ["destination","rw","type"])))
    ' "$inspection" >/dev/null 2>&1; then
        sudo -n rm -f -- "$inspection"
    fi
done < <(sudo -n find "$source_dir" -type f -name containers.json -print0)
# A failed candidate may have created links, devices, sockets, or FIFOs before
# the normal output validator ran. None are useful evidence and none may cross
# the retention boundary.
sudo -n find "$source_dir" -mindepth 1 ! -type f ! -type d -delete
sudo -n chown -R "$(id -u):$(id -g)" "$source_dir"

[ -z "$(find "$source_dir" -type d -name runtime -print -quit)" ] || die 'runtime secret directory survived sanitization'
[ -z "$(find "$source_dir" -type f \( -name '*.env' -o -name '*.env.new' \) -print -quit)" ] ||
    die 'environment secret file survived sanitization'
[ -z "$(find "$source_dir" -type f -name containers.json -print0 | while IFS= read -r -d '' inspection; do
    jq -e 'type == "array" and all(.[]; has("Config") | not)' "$inspection" >/dev/null 2>&1 || {
        printf '%s\n' "$inspection"
        break
    }
done)" ] || die 'raw container inspection survived sanitization'
[ -z "$(find "$source_dir" -mindepth 1 ! -type f ! -type d -print -quit)" ] ||
    die 'non-regular evidence survived sanitization'

failure_record="$source_dir/failure.json"
jq -n \
    --arg job_id "$FAILURE_JOB_ID" \
    --arg round_id "$FAILURE_ROUND_ID" \
    --argjson attempt "$FAILURE_ATTEMPT" \
    --argjson exit_code "$FAILURE_EXIT_CODE" \
    --argjson evaluator_line "$FAILURE_EVALUATOR_LINE" \
    '{schema:1,kind:"sim-latency-evaluator-failure",job_id:$job_id,
      round_id:$round_id,attempt:$attempt,exit_code:$exit_code,
      evaluator_line:$evaluator_line,sanitized:true,
      excluded:["input","scorer-input","score-runtime","evaluation-sources.*","runtime","*.env","*.env.new","non-regular entries"]}' \
    > "$failure_record"

find "$source_dir" -type d -exec chmod 0700 {} +
find "$source_dir" -type f -exec chmod 0400 {} +
install -d -m 0700 "$destination_dir"
cp -a "$source_dir/." "$destination_dir/"

records="$destination_parent/.failed-evidence-records"
manifest="$destination_parent/failed-evidence-manifest.json"
[ ! -e "$records" ] && [ ! -e "$manifest" ] || die 'failure manifest path already exists'
install -m 0600 /dev/null "$records"
while IFS= read -r -d '' path; do
    relative="${path#"$destination_parent/"}"
    jq -cn \
        --arg path "$relative" \
        --arg sha256 "$(sha256sum "$path" | awk '{print $1}')" \
        --argjson bytes "$(stat -c '%s' "$path")" \
        '{path:$path,sha256:$sha256,bytes:$bytes}' >> "$records"
done < <(find "$destination_dir" -type f -print0 | sort -z)
jq -s \
    --arg job_id "$FAILURE_JOB_ID" \
    --arg round_id "$FAILURE_ROUND_ID" \
    --argjson attempt "$FAILURE_ATTEMPT" \
    '{schema:1,kind:"sim-latency-failed-evidence-manifest",job_id:$job_id,
      round_id:$round_id,attempt:$attempt,sanitized:true,artifacts:.}' \
    "$records" > "$manifest"
rm -f -- "$records"

sync "$destination_dir" "$manifest"
find "$destination_dir" -type f -exec chmod 0400 {} +
find "$destination_dir" -type d -exec chmod 0500 {} +
chmod 0400 "$manifest"
