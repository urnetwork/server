#!/usr/bin/env bash
# Install root-owned alongside a root-owned fixed image/key configuration.
# Candidate stdin is never interpreted as a command or Docker option.
set -Eeuo pipefail
umask 077
export PATH=/usr/sbin:/usr/bin:/sbin:/bin
readonly configuration=/etc/urnetwork/competition-review.json
[[ $# == 0 ]] || { printf 'review broker accepts stdin only\n' >&2; exit 2; }
[[ -f "$configuration" && ! -L "$configuration" ]] || exit 1
[[ "$(stat -c %u "$configuration")" == 0 ]] || exit 1
permissions="$(stat -c %a "$configuration")"
(( (8#$permissions & 8#022) == 0 )) || exit 1
image="$(jq -er '.image_id' "$configuration")"
key="$(jq -er '.model_key_file' "$configuration")"
[[ "$image" =~ ^sha256:[0-9a-f]{64}$ ]] || exit 1
[[ "$key" == /var/lib/urnetwork/competition-review/model.key ]] || exit 1
[[ -f "$key" && ! -L "$key" ]] || exit 1
[[ "$(stat -c %a "$key")" == 400 ]] || exit 1
[[ "$(stat -c %u /var/lib/urnetwork/competition-review)" == 0 ]] || exit 1
[[ "$(stat -c %a /var/lib/urnetwork/competition-review)" == 700 ]] || exit 1
[[ "$(docker image inspect --format '{{.Id}}' "$image")" == "$image" ]] || exit 1
exec docker run --rm --interactive --init --pull never \
    --read-only --user 65532:65532 --cap-drop ALL \
    --security-opt no-new-privileges:true --cgroupns private \
    --pids-limit 64 --memory 2g --memory-swap 2g --cpus 1 \
    --cpuset-cpus 20,22 --ulimit nofile=1024:1024 \
    --mount "type=bind,source=$key,target=/run/secrets/model-key,readonly" \
    "$image"
