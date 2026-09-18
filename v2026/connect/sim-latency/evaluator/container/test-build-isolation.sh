#!/usr/bin/env bash

# Prove that candidate package initialization executed by the compile-only test
# cannot mutate trusted files in the final submission image.

set -Eeuo pipefail
umask 077

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
readonly PREPARE_EVALUATION_SOURCE="$SCRIPT_DIR/prepare-evaluation-source.sh"

base_image=""
policy_path=""
allow_local_base=false

usage() {
    printf '%s\n' \
        'usage: test-build-isolation.sh [--allow-local-base] --base-image IMAGE --policy FILE'
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --base-image|--policy)
            [ "$#" -ge 2 ] || { usage >&2; exit 2; }
            if [ "$1" = --base-image ]; then base_image="$2"; else policy_path="$2"; fi
            shift 2
            ;;
        --allow-local-base)
            allow_local_base=true
            shift
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            usage >&2
            exit 2
            ;;
    esac
done

[ -n "$base_image" ] && [ -n "$policy_path" ] || { usage >&2; exit 2; }
for command in git jq mktemp realpath sed sha256sum sudo; do
    command -v "$command" >/dev/null 2>&1 || {
        printf 'missing command: %s\n' "$command" >&2
        exit 1
    }
done
sudo -n docker info >/dev/null
[ -x "$PREPARE_EVALUATION_SOURCE" ] || {
    printf 'evaluation source preparer is not executable\n' >&2
    exit 1
}
[ -f "$policy_path" ] && [ ! -L "$policy_path" ] || {
    printf 'policy must be a regular non-symlink file\n' >&2
    exit 1
}
policy_path="$(realpath -e "$policy_path")"
if [ "$allow_local_base" != true ]; then
    [[ "$base_image" =~ @sha256:[0-9a-f]{64}$ ]] || {
        printf 'production isolation test requires a registry digest base\n' >&2
        exit 1
    }
fi
build_args=()
if [ "$allow_local_base" = true ]; then build_args+=(--allow-local-base); fi

test_root="$(mktemp -d "${TMPDIR:-/tmp}/urnetwork-build-isolation.XXXXXXXX")"
test_image=""
cleanup() {
    if [ -n "${test_image:-}" ]; then
        test_containers="$(sudo -n docker ps -aq --filter "ancestor=$test_image")"
        if [ -z "$test_containers" ]; then
            sudo -n docker image rm "$test_image" >/dev/null 2>&1 || true
        fi
    fi
    if [ -n "${test_root:-}" ] && [ -d "$test_root" ]; then
        chmod -R u+w "$test_root" 2>/dev/null || true
        rm -rf -- "$test_root"
    fi
}
trap cleanup EXIT INT TERM

base_image_id="$(sudo -n docker image inspect --format '{{.Id}}' "$base_image")"
evaluation_source_root="$test_root/evaluation-source"
$PREPARE_EVALUATION_SOURCE --base-image "$base_image_id" \
    --destination "$evaluation_source_root" >/dev/null
test -z "$(git -C "$evaluation_source_root/server" status --porcelain --untracked-files=all)"

# Even a deliberately malformed operator policy cannot open the trusted
# simulator tree. Exercise the builder's post-apply tree authentication, not
# only the API validator used on the normal submission path.
protected_target="$evaluation_source_root/server/connect/sim-latency/main.go"
printf '\n// protectedSimulatorTreeProbe must never survive patch validation.\n' >> "$protected_target"
protected_patch="$test_root/protected-sim-latency.patch"
git -C "$evaluation_source_root/server" diff --binary HEAD -- connect/sim-latency/main.go > "$protected_patch"
test -s "$protected_patch"
git -C "$evaluation_source_root/server" checkout -- connect/sim-latency/main.go
malformed_policy="$test_root/malformed-policy.json"
jq '.allowed_paths = ["connect/sim-latency/main.go"] |
    .forbidden_paths = ["unrelated/**"]' "$policy_path" > "$malformed_policy"
protected_tag="urnetwork/sim-latency-protected-reject:$(sha256sum "$protected_patch" | awk '{print substr($1,1,32)}')"
if "$SCRIPT_DIR/build-submission.sh" \
    "${build_args[@]}" \
    --base-image "$base_image" \
    --source-root "$evaluation_source_root" \
    --patch "$protected_patch" \
    --policy "$malformed_policy" \
    --tag "$protected_tag"; then
    printf 'builder accepted a patch to the protected sim-latency tree\n' >&2
    exit 1
fi
git -C "$evaluation_source_root/server" checkout -- connect/sim-latency/main.go
test -z "$(git -C "$evaluation_source_root/server" status --porcelain --untracked-files=all)"
! sudo -n docker image inspect "$protected_tag" >/dev/null 2>&1

target="$evaluation_source_root/server/connect/resident_contract_manager.go"
sed -i '/^import ($/a\	"os"' "$target"
printf '%s\n' \
    '' \
    '// buildIsolationProbe attempts the mutation that a root-run test stage' \
    '// would incorrectly preserve in the final candidate image.' \
    'func init() {' \
    '	for _, path := range []string{' \
    '		"/usr/local/libexec/competition-official-run",' \
    '		"/usr/local/libexec/competitionpatch",' \
    '		"/usr/local/libexec/competitiondbinit",' \
    '		"/opt/urnetwork/source-lock.json",' \
    '		"/workspace/server/go.mod",' \
    '	} {' \
    '		_ = os.WriteFile(path, []byte("candidate-init-corruption\n"), 0777)' \
    '	}' \
    '}' \
    >> "$target"

patch_path="$test_root/isolation.patch"
git -C "$evaluation_source_root/server" diff --binary HEAD -- connect/resident_contract_manager.go > "$patch_path"
test -s "$patch_path"
git -C "$evaluation_source_root/server" checkout -- connect/resident_contract_manager.go
patch_sha256="$(sha256sum "$patch_path" | awk '{print $1}')"
test_image="urnetwork/sim-latency-build-isolation:${patch_sha256:0:32}"

build_json="$(
    "$SCRIPT_DIR/build-submission.sh" \
        "${build_args[@]}" \
        --base-image "$base_image" \
        --source-root "$evaluation_source_root" \
        --patch "$patch_path" \
        --policy "$policy_path" \
        --tag "$test_image"
)"
candidate_image_id="$(jq -er '.image_id' <<<"$build_json")"
[ "$(sudo -n docker image inspect --format '{{.Id}}' "$test_image")" = "$candidate_image_id" ]

base_simulator_tree="$(
    sudo -n docker run --rm --network none --read-only --cap-drop ALL \
        --security-opt no-new-privileges:true --entrypoint /usr/bin/git \
        "$base_image_id" -C /workspace/server rev-parse HEAD:connect/sim-latency
)"
candidate_simulator_tree="$(
    sudo -n docker run --rm --network none --read-only --cap-drop ALL \
        --security-opt no-new-privileges:true --entrypoint /usr/bin/git \
        "$candidate_image_id" -C /workspace/server rev-parse HEAD:connect/sim-latency
)"
[ "$candidate_simulator_tree" = "$base_simulator_tree" ] || {
    printf 'candidate changed the protected sim-latency Git tree\n' >&2
    exit 1
}

readonly protected_paths=(
    /usr/local/libexec/competition-container
    /usr/local/libexec/competition-official-run
    /usr/local/libexec/competitionpatch
    /usr/local/libexec/competitiondbinit
    /opt/urnetwork/source-lock.json
    /workspace/server/go.mod
)
for path in "${protected_paths[@]}"; do
    base_hash="$(
        sudo -n docker run --rm --network none --read-only --cap-drop ALL \
            --security-opt no-new-privileges:true --entrypoint /usr/bin/sha256sum \
            "$base_image_id" "$path" | awk '{print $1}'
    )"
    candidate_hash="$(
        sudo -n docker run --rm --network none --read-only --cap-drop ALL \
            --security-opt no-new-privileges:true --entrypoint /usr/bin/sha256sum \
            "$candidate_image_id" "$path" | awk '{print $1}'
    )"
    [ "$candidate_hash" = "$base_hash" ] || {
        printf 'candidate initialization mutated protected path: %s\n' "$path" >&2
        exit 1
    }
done

[ "$(sudo -n docker image inspect --format '{{.Config.User}}' "$candidate_image_id")" = 65532:65532 ]
sudo -n docker run --rm --network none --read-only --cap-drop ALL \
    --security-opt no-new-privileges:true --entrypoint /bin/sh "$candidate_image_id" -eu -c \
    'test ! -e /opt/urnetwork/candidate-check
     test ! -e /opt/urnetwork/candidate-check.passed
     test ! -e /opt/urnetwork/candidate-build'

jq -n \
    --arg base_image_id "$base_image_id" \
    --arg candidate_image_id "$candidate_image_id" \
    --arg patch_sha256 "$patch_sha256" \
    --arg builder_sha256 "$(sha256sum "$SCRIPT_DIR/Dockerfile.submission" | awk '{print $1}')" \
    --argjson protected_path_count "${#protected_paths[@]}" \
    '{schema:1,status:"passed",base_image_id:$base_image_id,
      candidate_image_id:$candidate_image_id,patch_sha256:$patch_sha256,
      builder_sha256:$builder_sha256,checks_executed_unprivileged:true,
      check_stage_discarded:true,simulator_tree_immutable:true,
      protected_path_count:$protected_path_count}'
