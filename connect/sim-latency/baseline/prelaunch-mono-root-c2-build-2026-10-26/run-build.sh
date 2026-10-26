#!/usr/bin/env bash
# Qualified operator build entrypoint; no test or scoring command is executed.
set -Eeuo pipefail
umask 077
[[ ${1:-} == --execute-reviewed-c2-build && $# == 1 ]] || { printf 'usage: run-build.sh --execute-reviewed-c2-build\n' >&2; exit 2; }
readonly ROOT=/home/by/urnetwork/temp/mono-root-c2-release-20261026.PUHdHLML
readonly C2=a9e8e1a710b24c749e6499a0606938252c83e136
readonly C1=c1f9dda603d6279666fca5d92b99c5663306c5f7
readonly C1_IMAGE=sha256:002c39040ce23dee27b171c7e657f749c6a6d05b1e9b97d90ac87336470dbaf3
readonly BASE_TAG=urnetwork/sim-latency-evaluator-base:canceled-stop-c1f9dda6-go1273
readonly TAG=urnetwork/sim-latency-evaluator-base:mono-root-c2-a9e8e1a7-go1273
readonly SLOT=/home/by/urnetwork/temp/production-frontier-20261024.63AtMXS5/scoring.lock
export PATH=/home/by/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.27.3.linux-amd64/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export GOTOOLCHAIN=local GOMAXPROCS=2 GOFLAGS=-p=2
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_TERMINAL_PROMPT=0
if [[ ${C2_BUILD_MANAGEMENT_AFFINITY:-} != 20,22 ]]; then
    exec env C2_BUILD_MANAGEMENT_AFFINITY=20,22 taskset -c 20,22 "$0" "$@"
fi
[[ -f "$SLOT" && ! -L "$SLOT" ]]
exec 9<>"$SLOT"
flock -n 9
[[ ! -e "$ROOT/build.started" ]] || { printf 'build attempt already recorded; retain evidence before a separately authorized retry\n' >&2; exit 2; }
[[ $(git -C "$ROOT/source/server" rev-list --parents -n 1 HEAD) == "$C2 $C1" ]]
[[ $(sha256sum "$ROOT/source/server/connect/sim-latency/source_epoch.go" | cut -d' ' -f1) == d3f87c35204a8b4ebe6702558bcf9cad5b3c13e0a43060938f5159cf6f7d53bc ]]
[[ $(sha256sum "$ROOT/source/server/connect/sim-latency/source_epoch_mono_test.go" | cut -d' ' -f1) == 373ee1a94643e65280cdbd25ac9aabb3539680318cf1a01525699c2d254a9389 ]]
[[ $(sha256sum "$ROOT/source-lock.json" | cut -d' ' -f1) == 239cea7fe6f77f989b75ba6efa677e8731640ba2fdd3690a35e6fe72103f3d11 ]]
[[ $(sha256sum "$ROOT/sim-latency.yml" | cut -d' ' -f1) == 589ab624be8301c22e01571ac33b3e1b44e419ffb68009a577e6d520c0bb8995 ]]
canonical="$(jq -cj '{schema:.schema,development_snapshot:.development_snapshot,repositories:(.repositories|to_entries|sort_by(.key)|from_entries)}' "$ROOT/source-lock.json" | sha256sum | cut -d' ' -f1)"
[[ "$canonical" == ec322ed2db023e15ea5dc3eea60a6ca5989661bba737c340fea3477d8d1300cf ]]
for repository in server connect sdk proxy glog goidenticons userwireguard sn warp gvisor; do
    expected="$(jq -er --arg repository "$repository" '.repositories[$repository]' "$ROOT/source-lock.json")"
    [[ $(git -C "$ROOT/source/$repository" rev-parse HEAD) == "$expected" ]]
    [[ -z $(git -C "$ROOT/source/$repository" status --porcelain=v1 --untracked-files=all) ]]
done
base_id="$(sudo -n docker image inspect --format '{{.Id}}' "$BASE_TAG")"
[[ "$base_id" == "$C1_IMAGE" ]]
sudo -n docker image inspect --format '{{json .Config.OnBuild}}' "$BASE_TAG" | jq -e '(. // [] | length) == 0' >/dev/null
[[ $(sudo -n docker image inspect --format '{{.Architecture}}' "$BASE_TAG") == amd64 ]]
printf 'started_at=%s\nc2=%s\n' "$(date -u +%FT%TZ)" "$C2" > "$ROOT/build.started"
sha256sum "$ROOT/Dockerfile" "$ROOT/.dockerignore" "$ROOT/run-build.sh" "$ROOT/source-lock.json" "$ROOT/sim-latency.yml" > "$ROOT/build-inputs.sha256"
start_seconds=$SECONDS
set +e
DOCKER_BUILDKIT=1 timeout --signal=TERM --kill-after=60s 3600s \
    sudo -n docker build --platform linux/amd64 --provenance=false --pull=false --no-cache --network none \
    --cgroup-parent urnetwork-c2-build-a9e8e1a7.slice \
    --resource 'cpuset-cpus=20,22' --resource memory=12g --resource memory-swap=12g \
    --file "$ROOT/Dockerfile" --build-arg "C1_BASE_IMAGE=$BASE_TAG" \
    --build-arg "C2_SHA=$C2" \
    --build-arg SOURCE_LOCK_SHA256=239cea7fe6f77f989b75ba6efa677e8731640ba2fdd3690a35e6fe72103f3d11 \
    --build-arg SOURCE_CONFIG_SHA256=589ab624be8301c22e01571ac33b3e1b44e419ffb68009a577e6d520c0bb8995 \
    --tag "$TAG" "$ROOT" > "$ROOT/build.stdout.log" 2> "$ROOT/build.stderr.log"
status=$?
set -e
printf 'exit=%d\nduration_seconds=%d\nended_at=%s\n' "$status" "$((SECONDS-start_seconds))" "$(date -u +%FT%TZ)" > "$ROOT/build.terminal"
[[ "$status" == 0 ]] || exit "$status"
image_id="$(sudo -n docker image inspect --format '{{.Id}}' "$TAG")"
sudo -n docker image inspect "$image_id" > "$ROOT/image-inspect.json"
jq -n --arg c2 "$C2" --arg base_image_id "$C1_IMAGE" --arg image "$TAG" --arg image_id "$image_id" \
    --arg canonical_lock "$canonical" \
    '{schema:1,kind:"minimal-c2-image-build/v1",c2_commit:$c2,base_image_id:$base_image_id,image:$image,image_id:$image_id,canonical_source_lock_sha256:$canonical_lock,raw_source_lock_sha256:"239cea7fe6f77f989b75ba6efa677e8731640ba2fdd3690a35e6fe72103f3d11",source_config_sha256:"589ab624be8301c22e01571ac33b3e1b44e419ffb68009a577e6d520c0bb8995",tests_executed_by_builder:false,qualified:false,scoring_authorized:false}' > "$ROOT/image-record.json"
sha256sum "$ROOT/build.stdout.log" "$ROOT/build.stderr.log" "$ROOT/image-inspect.json" "$ROOT/image-record.json" > "$ROOT/build-outputs.sha256"
printf 'c2_image=%s\nqualification=pending_independent_low_tests_and_exact_image_checks\n' "$image_id"
