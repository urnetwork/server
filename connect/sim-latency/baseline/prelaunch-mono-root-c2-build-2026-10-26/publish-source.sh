#!/usr/bin/env bash
# Explicitly authorized administrative instrument publication; never force.
set -Eeuo pipefail
umask 077
readonly ROOT=/home/by/urnetwork/temp/mono-root-c2-release-20261026.PUHdHLML
readonly C1=c1f9dda603d6279666fca5d92b99c5663306c5f7
readonly C2=a9e8e1a710b24c749e6499a0606938252c83e136
[[ ${1:-} == --publish-approved-c2-source && $# == 1 ]] || exit 2
[[ ! -e "$ROOT/source-publication.started" ]]
for repository in server connect sdk proxy glog goidenticons userwireguard sn warp gvisor; do
    expected="$(jq -er --arg repository "$repository" '.repositories[$repository]' "$ROOT/c1-typed-lock.json")"
    observed="$(timeout --signal=TERM --kill-after=5s 60s git -c core.hooksPath=/dev/null -c gc.auto=0 -c maintenance.auto=false -C "/home/by/urnetwork/$repository" ls-remote --exit-code origin refs/heads/sim-latency)"
    [[ "$observed" == "$expected"$'\trefs/heads/sim-latency' ]]
    printf '%s\t%s\n' "$repository" "$observed"
done
origin="$(git -C /home/by/urnetwork/server remote get-url origin)"
[[ "$(timeout --signal=TERM --kill-after=5s 60s git -C /home/by/urnetwork/server ls-remote --exit-code origin refs/heads/sim-latency)" == "$C1"$'\trefs/heads/sim-latency' ]]
printf 'authorized_at=%s\nexpected_parent=%s\nnew_commit=%s\n' "$(date -u +%FT%TZ)" "$C1" "$C2" > "$ROOT/source-publication.started"
timeout --signal=TERM --kill-after=5s 120s git -c core.hooksPath=/dev/null -c gc.auto=0 -c maintenance.auto=false -C "$ROOT/source/server" push --porcelain "$origin" "$C2:refs/heads/sim-latency"
for repository in server connect sdk proxy glog goidenticons userwireguard sn warp gvisor; do
    expected="$(jq -er --arg repository "$repository" '.repositories[$repository]' "$ROOT/source-lock.json")"
    observed="$(timeout --signal=TERM --kill-after=5s 60s git -c core.hooksPath=/dev/null -c gc.auto=0 -c maintenance.auto=false -C "/home/by/urnetwork/$repository" ls-remote --exit-code origin refs/heads/sim-latency)"
    [[ "$observed" == "$expected"$'\trefs/heads/sim-latency' ]]
    printf '%s\t%s\n' "$repository" "$observed"
done
printf 'source_publication_complete=%s\nproduction_activated=false\nbridge_qualified=false\n' "$(date -u +%FT%TZ)"
