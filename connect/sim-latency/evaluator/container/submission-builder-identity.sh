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

subevaluation_scoped=false
if [[ -v URNETWORK_SUBEVALUATION_RUN_ID || -v URNETWORK_SUBEVALUATION_JOB_ID ||
      -v URNETWORK_SUBEVALUATION_ROUND_ID || -v URNETWORK_SUBEVALUATION_ATTEMPT ]]; then
    protected_file "$SCRIPT_DIR/subevaluation-ownership.sh"
    protected_file "$SCRIPT_DIR/compose.subevaluation.yml"
    protected_file "$SCRIPT_DIR/docker-id-map.sh"
    source "$SCRIPT_DIR/subevaluation-ownership.sh"
    subevaluation_ownership_init || fail
fi

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
if [[ "$subevaluation_scoped" = true ]]; then
    for leaf in Dockerfile.submission.tracked tracked-submission-build.sh docker-stock.seccomp.json candidate-core.seccomp.json compose.yml; do
        protected_file "$SCRIPT_DIR/$leaf"
    done
    ownership_sha256="$(sha256sum "$SCRIPT_DIR/subevaluation-ownership.sh" | awk '{print $1}')"
    compose_sha256="$(sha256sum "$SCRIPT_DIR/compose.subevaluation.yml" | awk '{print $1}')"
    id_map_sha256="$(sha256sum "$SCRIPT_DIR/docker-id-map.sh" | awk '{print $1}')"
    tracked_dockerfile_sha256="$(sha256sum "$SCRIPT_DIR/Dockerfile.submission.tracked" | awk '{print $1}')"
    tracked_builder_sha256="$(sha256sum "$SCRIPT_DIR/tracked-submission-build.sh" | awk '{print $1}')"
    stock_profile_sha256="$(sha256sum "$SCRIPT_DIR/docker-stock.seccomp.json" | awk '{print $1}')"
    core_profile_sha256="$(sha256sum "$SCRIPT_DIR/candidate-core.seccomp.json" | awk '{print $1}')"
    compose_base_sha256="$(sha256sum "$SCRIPT_DIR/compose.yml" | awk '{print $1}')"
    backing_images="$(awk '$1 == "image:" && ($2 ~ /^postgres:/ || $2 ~ /^redis:/) {print $2}' "$SCRIPT_DIR/compose.yml" |
        jq -Rsc 'split("\n")[:-1] | sort')"
    jq -e 'length == 2 and (unique | length == 2) and
        (map(select(test("^postgres:[a-zA-Z0-9_.-]+@sha256:[0-9a-f]{64}$"))) | length == 1) and
        (map(select(test("^redis:[a-zA-Z0-9_.-]+@sha256:[0-9a-f]{64}$"))) | length == 1)' \
        <<<"$backing_images" >/dev/null || fail
    manifest="$(jq -nSc --argjson manifest "$manifest" --arg ownership "$ownership_sha256" --arg compose "$compose_sha256" --arg id_map "$id_map_sha256" \
        --arg tracked_dockerfile "$tracked_dockerfile_sha256" --arg tracked_builder "$tracked_builder_sha256" \
        --arg stock_profile "$stock_profile_sha256" --arg core_profile "$core_profile_sha256" \
        --arg compose_base "$compose_base_sha256" --argjson backing_images "$backing_images" \
        '$manifest + {kind:"sim-latency-submission-builder/tracked-v1",execution_class:"tracked-create-core-one/v1",
          source_materialization:"required-readonly-run-source/v1",builtin_source_kind:"pristine-evaluator-base",
          subevaluation_ownership_sha256:$ownership,subevaluation_compose_sha256:$compose,subevaluation_docker_id_map_sha256:$id_map,
          tracked_dockerfile_sha256:$tracked_dockerfile,tracked_builder_sha256:$tracked_builder,
          stock_seccomp_profile_sha256:$stock_profile,candidate_core_profile_sha256:$core_profile,
          compose_base_sha256:$compose_base,backing_images:$backing_images}')"
fi
builder_sha256="$(printf '%s' "$manifest" | sha256sum | awk '{print $1}')"
jq -nSc --argjson manifest "$manifest" --arg builder_sha256 "$builder_sha256" \
    '$manifest + {builder_sha256:$builder_sha256}'
