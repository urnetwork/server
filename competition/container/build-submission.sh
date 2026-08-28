#!/usr/bin/env bash

# Build exactly one candidate image from a trusted evaluator base and the
# canonical patch/policy bytes accepted by the API. The build has no network.

set -Eeuo pipefail
umask 077

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
readonly RESOURCE_BOUNDARY="$SCRIPT_DIR/resource-boundary.sh"
readonly BUILD_TIMEOUT_SECONDS=600

base_image=""
patch_path=""
policy_path=""
source_root=""
image_tag=""
allow_local_base=false
original_args=("$@")

usage() {
    printf '%s\n' \
        'usage: build-submission.sh --base-image IMAGE@sha256:DIGEST --source-root DIR --patch FILE --policy FILE [--tag IMAGE]' \
        '       build-submission.sh --allow-local-base --base-image LOCAL_IMAGE --source-root DIR --patch FILE --policy FILE [--tag IMAGE]'
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --base-image|--source-root|--patch|--policy|--tag)
            [ "$#" -ge 2 ] || { usage >&2; exit 2; }
            case "$1" in
                --base-image) base_image="$2" ;;
                --source-root) source_root="$2" ;;
                --patch) patch_path="$2" ;;
                --policy) policy_path="$2" ;;
                --tag) image_tag="$2" ;;
            esac
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

[ -n "$base_image" ] && [ -n "$source_root" ] && [ -n "$patch_path" ] && [ -n "$policy_path" ] || { usage >&2; exit 2; }
if [ "$allow_local_base" != true ]; then
    [[ "$base_image" =~ @sha256:[0-9a-f]{64}$ ]] || {
        printf 'production base image must be a registry digest reference\n' >&2
        exit 1
    }
fi
for command in awk git jq sha256sum mktemp realpath sudo taskset timeout; do
    command -v "$command" >/dev/null 2>&1 || { printf 'missing command: %s\n' "$command" >&2; exit 1; }
done
sudo -n docker info >/dev/null
[ -x "$RESOURCE_BOUNDARY" ] || { printf 'trusted resource boundary is not executable\n' >&2; exit 1; }

resource_boundary_json="$($RESOURCE_BOUNDARY)"
evaluation_cpuset="$(jq -er '.evaluation_cpuset' <<<"$resource_boundary_json")"
management_cpuset="$(jq -er '.management_cpuset' <<<"$resource_boundary_json")"
build_memory_limit="$(jq -er '.build_memory_limit' <<<"$resource_boundary_json")"
expected_management_affinity="$(taskset -c "$management_cpuset" awk -F: \
    '$1 == "Cpus_allowed_list" {gsub(/[[:space:]]/, "", $2); print $2}' /proc/self/status)"
if [ "${COMPETITION_MANAGEMENT_CPUSET_APPLIED:-}" != "$management_cpuset" ]; then
    exec env COMPETITION_MANAGEMENT_CPUSET_APPLIED="$management_cpuset" \
        taskset -c "$management_cpuset" "$(realpath -e "$0")" "${original_args[@]}"
fi
actual_management_affinity="$(awk -F: \
    '$1 == "Cpus_allowed_list" {gsub(/[[:space:]]/, "", $2); print $2}' /proc/self/status)"
[ "$actual_management_affinity" = "$expected_management_affinity" ] || {
    printf 'submission builder is not confined to the dedicated management CPUs\n' >&2
    exit 1
}

[ -f "$patch_path" ] && [ ! -L "$patch_path" ] || { printf 'patch must be a regular non-symlink file\n' >&2; exit 1; }
[ -f "$policy_path" ] && [ ! -L "$policy_path" ] || { printf 'policy must be a regular non-symlink file\n' >&2; exit 1; }
[ -d "$source_root" ] && [ ! -L "$source_root" ] || { printf 'source root must be a directory, not a symlink\n' >&2; exit 1; }
patch_path="$(realpath -e "$patch_path")"
policy_path="$(realpath -e "$policy_path")"
source_root="$(realpath -e "$source_root")"

base_image_id="$(sudo -n docker image inspect --format '{{.Id}}' "$base_image")"
base_sha="$(sudo -n docker image inspect --format '{{index .Config.Labels "com.urnetwork.competition.base-sha"}}' "$base_image")"
image_kind="$(sudo -n docker image inspect --format '{{index .Config.Labels "com.urnetwork.competition.image-kind"}}' "$base_image")"
source_lock_sha256="$(sudo -n docker image inspect --format '{{index .Config.Labels "com.urnetwork.competition.source-lock-sha256"}}' "$base_image")"
[[ "$base_sha" =~ ^[0-9a-f]{40}$ ]] && [ "$image_kind" = evaluator-base ] || {
    printf 'base image is missing the trusted evaluator identity labels\n' >&2
    exit 1
}
[[ "$source_lock_sha256" =~ ^[0-9a-f]{64}$ ]] || {
    printf 'base image is missing its authenticated source lock\n' >&2
    exit 1
}

patch_sha256="$(sha256sum "$patch_path" | awk '{print $1}')"
policy_sha256="$(sha256sum "$policy_path" | awk '{print $1}')"
builder_sha256="$(sha256sum "$SCRIPT_DIR/Dockerfile.submission" | awk '{print $1}')"
build_context="$(mktemp -d "${TMPDIR:-/tmp}/urnetwork-submission.XXXXXXXX")"
cleanup() {
    if [ -n "${build_context:-}" ] && [ -d "$build_context" ]; then
        chmod -R u+w "$build_context" 2>/dev/null || true
        rm -rf -- "$build_context"
    fi
}
trap cleanup EXIT INT TERM

install -m 0400 "$patch_path" "$build_context/canonical.patch"
install -m 0400 "$policy_path" "$build_context/policy.json"

# Authenticate the freshly materialized checkout independently of the
# preparer. The checkout comes from the base image, never from the runner's
# repository worktrees, and every measured repository is on the temporary
# sim-latency branch at the source-lock commit.
source_identity="$source_root/.evaluation-source.json"
[ -f "$source_identity" ] && [ ! -L "$source_identity" ] || {
    printf 'evaluation source identity is missing\n' >&2
    exit 1
}
jq -e --arg base_image_id "$base_image_id" --arg base_sha "$base_sha" \
    --arg source_lock_sha256 "$source_lock_sha256" \
    '.schema == 1 and .kind == "sim-latency-evaluation-source" and
     .temporary == true and .base_image_id == $base_image_id and
     .base_sha == $base_sha and .branch == "sim-latency" and
     .source_lock_sha256 == $source_lock_sha256 and
     .candidate_patch_sha256 == null and
     ([.repositories.server,.repositories.connect,.repositories.sdk,.repositories.proxy] |
      all(type == "string" and test("^[0-9a-f]{40}$")))' \
    "$source_identity" >/dev/null || {
        printf 'evaluation source identity does not match the base image\n' >&2
        exit 1
    }
for repository in server connect sdk proxy; do
    repository_root="$source_root/$repository"
    [ -d "$repository_root/.git" ] && [ ! -L "$repository_root" ] || {
        printf 'evaluation source repository is missing or unsafe: %s\n' "$repository" >&2
        exit 1
    }
    expected_commit="$(jq -er --arg repository "$repository" '.repositories[$repository]' "$source_identity")"
    [ "$(git -C "$repository_root" symbolic-ref --quiet --short HEAD)" = sim-latency ] &&
        [ "$(git -C "$repository_root" rev-parse HEAD)" = "$expected_commit" ] &&
        [ -z "$(git -C "$repository_root" status --porcelain=v1 --untracked-files=all)" ] || {
            printf 'evaluation source repository does not match its temporary source identity: %s\n' "$repository" >&2
            exit 1
        }
done
[ "$(git -C "$source_root/server" rev-parse HEAD)" = "$base_sha" ]

# The simulator/scorer source is part of the trusted evaluator, never the
# submission surface. Authenticate its Git tree before and after applying the
# patch so this boundary fails closed independently of the API policy.
protected_simulator_tree="$(git -C "$source_root/server" rev-parse HEAD:connect/sim-latency)"
[[ "$protected_simulator_tree" =~ ^[0-9a-f]{40}$ ]] || {
    printf 'trusted sim-latency source tree is missing\n' >&2
    exit 1
}

# Apply and commit only inside the temporary candidate checkout. The same
# deterministic commit is independently reconstructed by Dockerfile.submission.
git -C "$source_root/server" apply --check --whitespace=error-all "$build_context/canonical.patch"
git -C "$source_root/server" apply --whitespace=error-all "$build_context/canonical.patch"
git -C "$source_root/server" diff --quiet -- connect/sim-latency || {
    printf 'submission attempted to modify the protected sim-latency source tree\n' >&2
    exit 1
}
[ -z "$(git -C "$source_root/server" status --porcelain=v1 --untracked-files=all -- connect/sim-latency)" ] || {
    printf 'submission created content in the protected sim-latency source tree\n' >&2
    exit 1
}
git -C "$source_root/server" add --update
git -C "$source_root/server" diff --cached --check
GIT_AUTHOR_NAME='URnetwork Competition' \
GIT_AUTHOR_EMAIL='competition@invalid' \
GIT_COMMITTER_NAME='URnetwork Competition' \
GIT_COMMITTER_EMAIL='competition@invalid' \
GIT_AUTHOR_DATE='2000-01-01T00:00:00Z' \
GIT_COMMITTER_DATE='2000-01-01T00:00:00Z' \
    git -C "$source_root/server" commit --quiet --no-gpg-sign -m "competition submission $patch_sha256"
candidate_sha="$(git -C "$source_root/server" rev-parse HEAD)"
[ "$(git -C "$source_root/server" symbolic-ref --quiet --short HEAD)" = sim-latency ]
[ -z "$(git -C "$source_root/server" status --porcelain=v1 --untracked-files=all)" ]
[ "$(git -C "$source_root/server" rev-parse HEAD:connect/sim-latency)" = "$protected_simulator_tree" ] || {
    printf 'submission modified the protected sim-latency source tree\n' >&2
    exit 1
}
chmod 0600 "$source_identity"
jq -S --arg candidate_sha "$candidate_sha" --arg patch_sha256 "$patch_sha256" \
    '.repositories.server = $candidate_sha |
     .candidate_patch_sha256 = $patch_sha256' \
    "$source_identity" > "$source_identity.new"
mv -f -- "$source_identity.new" "$source_identity"
chmod 0400 "$source_identity"

image_key="$(printf '%s\000%s\000%s\000%s' \
    "$base_image_id" "$patch_sha256" "$policy_sha256" "$builder_sha256" | sha256sum | awk '{print $1}')"
build_cgroup_parent="urnetwork-submission-build-${image_key:0:32}.slice"
if [ -z "$image_tag" ]; then
    image_tag="urnetwork/sim-latency-submission:${image_key:0:32}"
fi

# A cache hit is accepted only when every trusted identity label agrees. A
# caller-supplied tag that points at unrelated content fails closed.
if sudo -n docker image inspect "$image_tag" >/dev/null 2>&1; then
    image_id="$(sudo -n docker image inspect --format '{{.Id}}' "$image_tag")"
    cached_identity="$(sudo -n docker image inspect "$image_tag" | jq -er '.[0].Config.Labels')"
    jq -e \
        --arg base_sha "$base_sha" \
        --arg candidate_sha "$candidate_sha" \
        --arg patch_sha256 "$patch_sha256" \
        --arg policy_sha256 "$policy_sha256" \
        --arg builder_sha256 "$builder_sha256" \
        --arg image_key "$image_key" \
        '."com.urnetwork.competition.image-kind" == "submission" and
         ."com.urnetwork.competition.base-sha" == $base_sha and
         ."org.opencontainers.image.revision" == $candidate_sha and
         ."com.urnetwork.competition.patch-sha256" == $patch_sha256 and
         ."com.urnetwork.competition.policy-sha256" == $policy_sha256 and
         ."com.urnetwork.competition.builder-sha256" == $builder_sha256 and
         ."com.urnetwork.competition.image-key" == $image_key' \
        <<<"$cached_identity" >/dev/null || {
            printf 'existing candidate tag does not match authenticated build inputs: %s\n' "$image_tag" >&2
            exit 1
        }
else
DOCKER_BUILDKIT=1 timeout --signal=TERM --kill-after=30s "$BUILD_TIMEOUT_SECONDS" \
    sudo -n docker build \
    --platform linux/amd64 \
    --provenance=false \
    --pull=false \
    --network none \
    --cgroup-parent "$build_cgroup_parent" \
    --resource "cpuset-cpus=$evaluation_cpuset" \
    --resource "memory=$build_memory_limit" \
    --resource "memory-swap=$build_memory_limit" \
    --file "$SCRIPT_DIR/Dockerfile.submission" \
    --build-arg "EVALUATOR_BASE_IMAGE=$base_image" \
    --build-arg "BASE_SHA=$base_sha" \
    --build-arg "CANDIDATE_SHA=$candidate_sha" \
    --build-arg "PATCH_SHA256=$patch_sha256" \
    --build-arg "POLICY_SHA256=$policy_sha256" \
    --build-arg "BUILDER_SHA256=$builder_sha256" \
    --build-arg "SUBMISSION_IDENTITY=$image_key" \
    --tag "$image_tag" \
    "$build_context" >&2

image_id="$(sudo -n docker image inspect --format '{{.Id}}' "$image_tag")"
fi

actual_image_id="$(sudo -n docker image inspect --format '{{.Id}}' "$image_tag")"
[ "$actual_image_id" = "$image_id" ] || {
    printf 'candidate image id changed after build\n' >&2
    exit 1
}
actual_labels="$(sudo -n docker image inspect "$image_id" | jq -er '.[0].Config.Labels')"
jq -e \
    --arg base_sha "$base_sha" \
    --arg candidate_sha "$candidate_sha" \
    --arg patch_sha256 "$patch_sha256" \
    --arg policy_sha256 "$policy_sha256" \
    --arg builder_sha256 "$builder_sha256" \
    --arg image_key "$image_key" \
    '."com.urnetwork.competition.image-kind" == "submission" and
     ."com.urnetwork.competition.base-sha" == $base_sha and
     ."org.opencontainers.image.revision" == $candidate_sha and
     ."com.urnetwork.competition.patch-sha256" == $patch_sha256 and
     ."com.urnetwork.competition.policy-sha256" == $policy_sha256 and
     ."com.urnetwork.competition.builder-sha256" == $builder_sha256 and
     ."com.urnetwork.competition.image-key" == $image_key' \
    <<<"$actual_labels" >/dev/null || {
        printf 'candidate image labels do not authenticate build inputs\n' >&2
        exit 1
    }
runtime_identity="$(sudo -n docker run --rm \
    --network none \
    --read-only \
    --cap-drop ALL \
    --security-opt no-new-privileges:true \
    "$image_id" identity)"
jq -e \
    --arg base_sha "$base_sha" \
    --arg candidate_sha "$candidate_sha" \
    --arg patch_sha256 "$patch_sha256" \
    --arg policy_sha256 "$policy_sha256" \
    --arg builder_sha256 "$builder_sha256" \
    --arg image_key "$image_key" \
    '.schema == 1 and .image_kind == "submission" and
     .base_sha == $base_sha and .build_sha == $candidate_sha and
     .patch_sha256 == $patch_sha256 and .policy_sha256 == $policy_sha256 and
     .builder_sha256 == $builder_sha256 and .image_key == $image_key and
     (.simulator_sha256 | test("^[0-9a-f]{64}$")) and (.paths | type == "array")' \
    <<<"$runtime_identity" >/dev/null || {
        printf 'candidate runtime identity does not authenticate build inputs\n' >&2
        exit 1
    }

jq -n \
    --arg image "$image_tag" \
    --arg image_id "$image_id" \
    --arg base_image_id "$base_image_id" \
    --arg base_sha "$base_sha" \
    --arg candidate_sha "$candidate_sha" \
    --arg patch_sha256 "$patch_sha256" \
    --arg policy_sha256 "$policy_sha256" \
    --arg builder_sha256 "$builder_sha256" \
    --arg image_key "$image_key" \
    '{schema: 1, image: $image, image_id: $image_id,
      base_image_id: $base_image_id, base_sha: $base_sha,
      candidate_sha: $candidate_sha, patch_sha256: $patch_sha256,
      policy_sha256: $policy_sha256, builder_sha256: $builder_sha256,
      image_key: $image_key}'
