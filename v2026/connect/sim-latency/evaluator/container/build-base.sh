#!/usr/bin/env bash

# Build the trusted evaluator base from clean, secret-free repository clones.
# Production uses committed HEADs. --include-worktree exists only to exercise a
# not-yet-committed evaluator during development; it creates deterministic
# synthetic commits inside the temporary build context and never alters a
# source checkout.

set -Eeuo pipefail
umask 077

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
readonly SERVER_ROOT="$(cd "$SCRIPT_DIR/../../../.." && pwd -P)"
readonly WORKSPACE_ROOT="$(cd "$SERVER_ROOT/.." && pwd -P)"

include_worktree=false
image_tag=""
source_epoch=""
source_config="$WORKSPACE_ROOT/config/main/sim-latency.yml"

usage() {
    printf '%s\n' 'usage: build-base.sh --epoch 0..6 [--source-config FILE] [--include-worktree] [--tag IMAGE]'
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --include-worktree)
            include_worktree=true
            shift
            ;;
        --epoch|--source-config|--tag)
            [ "$#" -ge 2 ] || { usage >&2; exit 2; }
            case "$1" in
                --epoch) source_epoch="$2" ;;
                --source-config) source_config="$2" ;;
                --tag) image_tag="$2" ;;
            esac
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

[[ "$source_epoch" =~ ^[0-6]$ ]] || { usage >&2; exit 2; }
[ -f "$source_config" ] && [ ! -L "$source_config" ] || {
    printf 'source epoch config must be a regular non-symlink file: %s\n' "$source_config" >&2
    exit 1
}
source_config="$(realpath -e "$source_config")"
config_root="$(git -C "$(dirname "$source_config")" rev-parse --show-toplevel)" || {
    printf 'source epoch config is not in a git repository\n' >&2
    exit 1
}
source_config_relative="$(realpath --relative-to="$config_root" "$source_config")"
git -C "$config_root" ls-files --error-unmatch -- "$source_config_relative" >/dev/null || {
    printf 'source epoch config is not committed: %s\n' "$source_config" >&2
    exit 1
}
git -C "$config_root" diff --quiet HEAD -- "$source_config_relative" &&
    git -C "$config_root" diff --cached --quiet -- "$source_config_relative" || {
    printf 'source epoch config has uncommitted changes: %s\n' "$source_config" >&2
    exit 1
}

for command in git go jq sha256sum mktemp realpath stat sudo; do
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
install -m 0444 "$source_config" "$build_context/sim-latency.yml"

readonly REPOSITORIES=(server connect proxy sdk glog goidenticons userwireguard sn)
readonly MEASURED_REPOSITORIES=(server connect proxy sdk)
declare -A revisions

source_record=""
if [ "$include_worktree" = false ]; then
    source_tool="${SIM_LATENCY_SOURCE_TOOL:-}"
    if [ -n "$source_tool" ]; then
        [ -x "$source_tool" ] && [ ! -L "$source_tool" ] || {
            printf 'SIM_LATENCY_SOURCE_TOOL must be a regular executable: %s\n' "$source_tool" >&2
            exit 1
        }
        source_record="$("$source_tool" source-record \
            --epoch "$source_epoch" \
            --source-config "$source_config" \
            --repos-root "$WORKSPACE_ROOT")"
    else
        source_record="$(cd "$SERVER_ROOT/connect/sim-latency" && go run . source-record \
            --epoch "$source_epoch" \
            --source-config "$source_config" \
            --repos-root "$WORKSPACE_ROOT")"
    fi
    jq -e \
        --argjson epoch "$source_epoch" \
        '.schema == 1 and .epoch == $epoch and .branch == "sim-latency" and
         (.significant_improvement_percent | type == "number" and . > 0 and . <= 50) and
         (["server","connect","proxy","sdk"] |
          all(. as $repository | $record.repositories[$repository] | test("^[0-9a-f]{40}$")))' \
        --argjson record "$source_record" \
        <<<"$source_record" >/dev/null || {
        printf 'authenticated source record is malformed\n' >&2
        exit 1
    }
fi

is_measured_repository() {
    local candidate="$1" repository
    for repository in "${MEASURED_REPOSITORIES[@]}"; do
        [ "$candidate" = "$repository" ] && return 0
    done
    return 1
}

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
    if [ "$include_worktree" = false ] && is_measured_repository "$repository"; then
        source_revision="$(jq -er --arg repository "$repository" '.repositories[$repository]' <<<"$source_record")"
        source_origin="$(git -C "$source_root" remote get-url origin)"
    else
        source_revision="$(git -C "$source_root" rev-parse HEAD)"
        source_origin="file://$source_root"
    fi
    git init --quiet "$clone_root"
    git -C "$clone_root" remote add origin "$source_origin"
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
source_config_sha256="$(sha256sum "$build_context/sim-latency.yml" | awk '{print $1}')"
if [ -z "$image_tag" ]; then
    image_tag="urnetwork/sim-latency-evaluator-base:${base_sha:0:24}"
fi

DOCKER_BUILDKIT=1 sudo -n docker build \
    --platform linux/amd64 \
    --network default \
    --file "$SCRIPT_DIR/Dockerfile.base" \
    --build-arg "BASE_SHA=$base_sha" \
    --build-arg "SOURCE_EPOCH=$source_epoch" \
    --build-arg "SOURCE_CONFIG_SHA256=$source_config_sha256" \
    --build-arg "SOURCE_LOCK_SHA256=$source_lock_sha256" \
    --label "com.urnetwork.competition.development-snapshot=$include_worktree" \
    --label "com.urnetwork.competition.source-epoch=$source_epoch" \
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
    --argjson source_epoch "$source_epoch" \
    --arg source_config_sha256 "$source_config_sha256" \
    --arg source_lock_sha256 "$source_lock_sha256" \
    --argjson development_snapshot "$include_worktree" \
    '{schema: 1, image: $image, image_id: $image_id, base_sha: $base_sha,
      source_epoch: $source_epoch, source_config_sha256: $source_config_sha256,
      source_lock_sha256: $source_lock_sha256,
      development_snapshot: $development_snapshot}'
