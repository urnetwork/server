#!/usr/bin/env bash

# Build the trusted evaluator base from clean, secret-free repository clones.
# Production uses committed HEADs. --include-worktree exists only to exercise a
# not-yet-committed evaluator during development; it creates deterministic
# synthetic commits inside the temporary build context and never alters a
# source checkout.

set -Eeuo pipefail
umask 077

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
readonly SERVER_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd -P)"
readonly WORKSPACE_ROOT="$(cd "$SERVER_ROOT/.." && pwd -P)"

include_worktree=false
image_tag=""

usage() {
    printf '%s\n' 'usage: build-base.sh [--include-worktree] [--tag IMAGE]'
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --include-worktree)
            include_worktree=true
            shift
            ;;
        --tag)
            [ "$#" -ge 2 ] || { usage >&2; exit 2; }
            image_tag="$2"
            shift 2
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

for command in git jq sha256sum mktemp stat sudo; do
    command -v "$command" >/dev/null 2>&1 || { printf 'missing command: %s\n' "$command" >&2; exit 1; }
done
sudo -n docker info >/dev/null

build_context="$(mktemp -d "${TMPDIR:-/tmp}/urnetwork-evaluator-base.XXXXXXXX")"
cleanup() {
    if [ -n "${build_context:-}" ] && [ -d "$build_context" ]; then
        chmod -R u+w "$build_context" 2>/dev/null || true
        rm -rf -- "$build_context"
    fi
}
trap cleanup EXIT INT TERM

install -d -m 0700 "$build_context/source" "$build_context/evaluator"
install -m 0555 "$SCRIPT_DIR/entrypoint.sh" "$build_context/evaluator/entrypoint.sh"
install -m 0555 "$SERVER_ROOT/connect/sim-latency/official-run.sh" "$build_context/evaluator/official-run.sh"

readonly REPOSITORIES=(server connect proxy sdk glog goidenticons userwireguard sn)
declare -A revisions

overlay_worktree() {
    local source_root="$1" clone_root="$2" relative
    if ! git -C "$source_root" diff --quiet HEAD; then
        git -C "$source_root" diff --binary HEAD | git -C "$clone_root" apply --whitespace=nowarn
    fi
    while IFS= read -r -d '' relative; do
        [ -f "$source_root/$relative" ] && [ ! -L "$source_root/$relative" ] || {
            printf 'development snapshot only accepts regular untracked files: %s/%s\n' "$source_root" "$relative" >&2
            exit 1
        }
        install -D -m "$(stat -c '%a' "$source_root/$relative")" "$source_root/$relative" "$clone_root/$relative"
    done < <(git -C "$source_root" ls-files --others --exclude-standard -z)
    if [ -n "$(git -C "$clone_root" status --porcelain --untracked-files=all)" ]; then
        git -C "$clone_root" add --all
        GIT_AUTHOR_NAME='URnetwork Evaluator' \
        GIT_AUTHOR_EMAIL='evaluator@invalid' \
        GIT_COMMITTER_NAME='URnetwork Evaluator' \
        GIT_COMMITTER_EMAIL='evaluator@invalid' \
        GIT_AUTHOR_DATE='1999-12-31T00:00:00Z' \
        GIT_COMMITTER_DATE='1999-12-31T00:00:00Z' \
            git -C "$clone_root" commit --quiet --no-gpg-sign -m 'evaluator development snapshot'
    fi
}

for repository in "${REPOSITORIES[@]}"; do
    source_root="$WORKSPACE_ROOT/$repository"
    clone_root="$build_context/source/$repository"
    [ -d "$source_root/.git" ] || { printf 'repository missing: %s\n' "$source_root" >&2; exit 1; }
    source_revision="$(git -C "$source_root" rev-parse HEAD)"
    git init --quiet "$clone_root"
    git -C "$clone_root" remote add origin "file://$source_root"
    git -C "$clone_root" fetch --quiet --depth=1 origin "$source_revision"
    git -C "$clone_root" checkout --quiet --detach FETCH_HEAD
    if [ "$include_worktree" = true ]; then
        overlay_worktree "$source_root" "$clone_root"
    fi
    [ -z "$(git -C "$clone_root" status --porcelain --untracked-files=all)" ] || {
        printf 'temporary clone is not clean: %s\n' "$repository" >&2
        exit 1
    }
    revisions[$repository]="$(git -C "$clone_root" rev-parse HEAD)"
done

jq -n \
    --arg server "${revisions[server]}" \
    --arg connect "${revisions[connect]}" \
    --arg proxy "${revisions[proxy]}" \
    --arg sdk "${revisions[sdk]}" \
    --arg glog "${revisions[glog]}" \
    --arg goidenticons "${revisions[goidenticons]}" \
    --arg userwireguard "${revisions[userwireguard]}" \
    --arg sn "${revisions[sn]}" \
    --argjson development_snapshot "$include_worktree" \
    '{schema: 1, development_snapshot: $development_snapshot,
      repositories: {server: $server, connect: $connect, proxy: $proxy,
      sdk: $sdk, glog: $glog, goidenticons: $goidenticons,
      userwireguard: $userwireguard, sn: $sn}}' \
    > "$build_context/source-lock.json"

base_sha="${revisions[server]}"
source_lock_sha256="$(sha256sum "$build_context/source-lock.json" | awk '{print $1}')"
if [ -z "$image_tag" ]; then
    image_tag="urnetwork/sim-latency-evaluator-base:${base_sha:0:24}"
fi

DOCKER_BUILDKIT=1 sudo -n docker build \
    --platform linux/amd64 \
    --network default \
    --file "$SCRIPT_DIR/Dockerfile.base" \
    --build-arg "BASE_SHA=$base_sha" \
    --build-arg "SOURCE_LOCK_SHA256=$source_lock_sha256" \
    --label "com.urnetwork.competition.development-snapshot=$include_worktree" \
    --tag "$image_tag" \
    "$build_context" >&2

image_id="$(sudo -n docker image inspect --format '{{.Id}}' "$image_tag")"
sudo -n docker run --rm \
    --network none \
    --read-only \
    --cap-drop ALL \
    --security-opt no-new-privileges:true \
    "$image_id" identity >/dev/null

jq -n \
    --arg image "$image_tag" \
    --arg image_id "$image_id" \
    --arg base_sha "$base_sha" \
    --arg source_lock_sha256 "$source_lock_sha256" \
    --argjson development_snapshot "$include_worktree" \
    '{schema: 1, image: $image, image_id: $image_id, base_sha: $base_sha,
      source_lock_sha256: $source_lock_sha256,
      development_snapshot: $development_snapshot}'
