#!/usr/bin/env bash
# Authenticate one immutable trusted builder release. No candidate path, model
# credential, environment-selected executable or mutable base-image tool is used.
set -Eeuo pipefail
export PATH=/usr/sbin:/usr/bin:/sbin:/bin
readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
[[ $# == 0 ]] || exit 2

fail() { printf 'submission builder release is absent, unsafe or unpinned\n' >&2; exit 2; }

protected_file() {
    local leaf="$1" current="$1" owner permissions
    [[ -f "$leaf" && ! -L "$leaf" ]] || fail
    while :; do
        [[ ! -L "$current" ]] || fail
        owner="$(stat -c %u "$current")" || fail
        permissions="$(stat -c %a "$current")" || fail
        [[ "$owner" == 0 && "$permissions" =~ ^[0-7]{3,4}$ ]] || fail
        (( (8#$permissions & 8#022) == 0 )) || fail
        [[ "$current" != / ]] || break
        current="$(dirname "$current")"
        [[ -d "$current" ]] || fail
    done
}

for leaf in submission-builder-identity.sh build-submission.sh Dockerfile.submission evaluator.sh prepare-evaluation-source.sh competitionpatch competitionpatch.sha256; do
    protected_file "$SCRIPT_DIR/$leaf"
done
[[ -x "$SCRIPT_DIR/competitionpatch" ]] || fail
[[ "$(stat -c %s "$SCRIPT_DIR/competitionpatch.sha256")" -le 65 ]] || fail
expected_validator_sha256="$(<"$SCRIPT_DIR/competitionpatch.sha256")"
[[ "$expected_validator_sha256" =~ ^[0-9a-f]{64}$ ]] || fail
validator_sha256="$(sha256sum "$SCRIPT_DIR/competitionpatch" | awk '{print $1}')"
[[ "$validator_sha256" == "$expected_validator_sha256" ]] || fail
dockerfile_sha256="$(sha256sum "$SCRIPT_DIR/Dockerfile.submission" | awk '{print $1}')"
build_script_sha256="$(sha256sum "$SCRIPT_DIR/build-submission.sh" | awk '{print $1}')"
identity_helper_sha256="$(sha256sum "$SCRIPT_DIR/submission-builder-identity.sh" | awk '{print $1}')"
validator_pin_sha256="$(sha256sum "$SCRIPT_DIR/competitionpatch.sha256" | awk '{print $1}')"
evaluator_sha256="$(sha256sum "$SCRIPT_DIR/evaluator.sh" | awk '{print $1}')"
source_preparer_sha256="$(sha256sum "$SCRIPT_DIR/prepare-evaluation-source.sh" | awk '{print $1}')"
manifest="$(jq -nSc --arg validator "$validator_sha256" --arg dockerfile "$dockerfile_sha256" \
    --arg build_script "$build_script_sha256" --arg identity_helper "$identity_helper_sha256" \
    --arg validator_pin "$validator_pin_sha256" --arg evaluator "$evaluator_sha256" \
    --arg source_preparer "$source_preparer_sha256" \
    '{schema:1,kind:"sim-latency-submission-builder/v2",patch_validator_sha256:$validator,
      dockerfile_sha256:$dockerfile,build_script_sha256:$build_script,
      identity_helper_sha256:$identity_helper,validator_pin_sha256:$validator_pin,
      evaluator_sha256:$evaluator,source_preparer_sha256:$source_preparer}')"
builder_sha256="$(printf '%s' "$manifest" | sha256sum | awk '{print $1}')"
jq -nSc --argjson manifest "$manifest" --arg builder_sha256 "$builder_sha256" \
    '$manifest + {builder_sha256:$builder_sha256}'
