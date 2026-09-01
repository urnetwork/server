#!/usr/bin/env bash

# Materialize one clean evaluation source tree from an authenticated evaluator
# image. Host repository worktrees are deliberately not inputs: an evaluation
# remains reproducible while the API/worker checkout advances on main or has
# unrelated local changes.

set -Eeuo pipefail
umask 077
export LANG=C LC_ALL=C

readonly REPOSITORIES=(server connect sdk proxy)

base_image=""
destination=""
source_container=""
source_lock_path=""

usage() {
    printf '%s\n' \
        'usage: prepare-evaluation-source.sh --base-image IMAGE --destination ABSOLUTE_DIRECTORY'
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --base-image|--destination)
            [ "$#" -ge 2 ] || { usage >&2; exit 2; }
            case "$1" in
                --base-image) base_image="$2" ;;
                --destination) destination="$2" ;;
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

[ -n "$base_image" ] && [ -n "$destination" ] || { usage >&2; exit 2; }
for command in git install jq mktemp realpath sha256sum sudo; do
    command -v "$command" >/dev/null 2>&1 || {
        printf 'missing command: %s\n' "$command" >&2
        exit 1
    }
done
sudo -n docker info >/dev/null

[[ "$destination" = /* ]] || {
    printf 'evaluation source destination must be absolute\n' >&2
    exit 1
}
destination_parent="$(realpath -e "$(dirname "$destination")")"
destination_name="$(basename "$destination")"
[ "$destination_name" != . ] && [ "$destination_name" != .. ] && [ "$destination_name" != / ] || {
    printf 'evaluation source destination is unsafe\n' >&2
    exit 1
}
destination="$destination_parent/$destination_name"
[ ! -e "$destination" ] && [ ! -L "$destination" ] || {
    printf 'evaluation source destination already exists: %s\n' "$destination" >&2
    exit 1
}

cleanup() {
    local rc=$?
    if [ -n "${source_container:-}" ]; then
        sudo -n docker rm -f "$source_container" >/dev/null 2>&1 || true
    fi
    if [ -n "${source_lock_path:-}" ]; then
        rm -f -- "$source_lock_path"
    fi
    if [ "$rc" -ne 0 ] && [ -n "${destination:-}" ] && [ -d "$destination" ]; then
        sudo -n chmod -R u+rwX "$destination" >/dev/null 2>&1 || true
        sudo -n rm -rf -- "$destination"
    fi
    exit "$rc"
}
trap cleanup EXIT INT TERM

base_image_id="$(sudo -n docker image inspect --format '{{.Id}}' "$base_image")"
image_kind="$(sudo -n docker image inspect --format '{{index .Config.Labels "com.urnetwork.competition.image-kind"}}' "$base_image_id")"
base_sha="$(sudo -n docker image inspect --format '{{index .Config.Labels "com.urnetwork.competition.base-sha"}}' "$base_image_id")"
source_epoch="$(sudo -n docker image inspect --format '{{index .Config.Labels "com.urnetwork.competition.source-epoch"}}' "$base_image_id")"
expected_source_lock_sha256="$(sudo -n docker image inspect --format '{{index .Config.Labels "com.urnetwork.competition.source-lock-sha256"}}' "$base_image_id")"
[ "$image_kind" = evaluator-base ] && [[ "$base_sha" =~ ^[0-9a-f]{40}$ ]] &&
    [[ "$source_epoch" =~ ^[0-6]$ ]] && [[ "$expected_source_lock_sha256" =~ ^[0-9a-f]{64}$ ]] || {
        printf 'base image is missing its evaluator source identity\n' >&2
        exit 1
    }

install -d -m 0700 "$destination"
source_lock_path="$(mktemp "$destination_parent/.evaluation-source-lock.XXXXXXXX.json")"
source_container="$(sudo -n docker create "$base_image_id")"
sudo -n docker cp "$source_container:/opt/urnetwork/source-lock.json" "$source_lock_path"
for repository in "${REPOSITORIES[@]}"; do
    sudo -n docker cp "$source_container:/workspace/$repository" "$destination/"
done
sudo -n docker rm "$source_container" >/dev/null
source_container=""
sudo -n chown -R "$(id -u):$(id -g)" "$destination" "$source_lock_path"

source_lock_sha256="$(sha256sum "$source_lock_path" | awk '{print $1}')"
[ "$source_lock_sha256" = "$expected_source_lock_sha256" ] || {
    printf 'evaluator source lock digest mismatch\n' >&2
    exit 1
}
jq -e '
    type == "object" and .schema == 1 and
    (.development_snapshot | type == "boolean") and
    (.repositories | type == "object") and
    (["server","connect","sdk","proxy"] |
      all(. as $repository |
        $lock.repositories[$repository] | test("^[0-9a-f]{40}$")))
' --argjson lock "$(jq -c . "$source_lock_path")" "$source_lock_path" >/dev/null || {
    printf 'evaluator source lock is malformed\n' >&2
    exit 1
}

for repository in "${REPOSITORIES[@]}"; do
    repository_root="$destination/$repository"
    [ -d "$repository_root/.git" ] && [ ! -L "$repository_root" ] || {
        printf 'evaluation repository is missing or unsafe: %s\n' "$repository" >&2
        exit 1
    }
    expected_commit="$(jq -er --arg repository "$repository" '.repositories[$repository]' "$source_lock_path")"
    [ "$(git -C "$repository_root" rev-parse HEAD)" = "$expected_commit" ] || {
        printf 'evaluation repository %s does not match the source lock\n' "$repository" >&2
        exit 1
    }
    [ -z "$(git -C "$repository_root" status --porcelain=v1 --untracked-files=all)" ] || {
        printf 'evaluation repository is not clean: %s\n' "$repository" >&2
        exit 1
    }
    git -C "$repository_root" checkout --quiet -B sim-latency "$expected_commit"
    [ "$(git -C "$repository_root" symbolic-ref --quiet --short HEAD)" = sim-latency ]
    [ "$(git -C "$repository_root" rev-parse HEAD)" = "$expected_commit" ]
    [ -z "$(git -C "$repository_root" status --porcelain=v1 --untracked-files=all)" ]
done

identity_path="$destination/.evaluation-source.json"
jq -nS \
    --arg base_image_id "$base_image_id" \
    --arg base_sha "$base_sha" \
    --argjson source_epoch "$source_epoch" \
    --arg source_lock_sha256 "$source_lock_sha256" \
    --arg server "$(git -C "$destination/server" rev-parse HEAD)" \
    --arg connect "$(git -C "$destination/connect" rev-parse HEAD)" \
    --arg sdk "$(git -C "$destination/sdk" rev-parse HEAD)" \
    --arg proxy "$(git -C "$destination/proxy" rev-parse HEAD)" \
    '{schema:1,kind:"sim-latency-evaluation-source",temporary:true,
      base_image_id:$base_image_id,base_sha:$base_sha,source_epoch:$source_epoch,
      branch:"sim-latency",source_lock_sha256:$source_lock_sha256,
      repositories:{server:$server,connect:$connect,sdk:$sdk,proxy:$proxy},
      candidate_patch_sha256:null}' > "$identity_path"
chmod 0400 "$identity_path"
rm -f -- "$source_lock_path"
source_lock_path=""
jq --sort-keys . "$identity_path"
