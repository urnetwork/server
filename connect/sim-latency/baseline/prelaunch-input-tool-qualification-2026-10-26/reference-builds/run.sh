#!/bin/bash
set -Eeuo pipefail
root=/home/by/urnetwork/temp/reference-image-readback-20261026.Kyj1R65B
exec 9<>/home/by/urnetwork/temp/production-frontier-20261024.63AtMXS5/scoring.lock
flock -n 9
cleanup() {
 status=$?;trap - EXIT
 for arm in noop snapshot negative; do
  if [[ -s $root/$arm.cid ]]; then
   id=$(< "$root/$arm.cid")
   found=$(docker ps -aq --no-trunc --filter "id=$id")
   if [[ -n $found ]]; then
    [[ $(docker inspect "$id" --format '{{index .Config.Labels "urnetwork.fixture.owner"}}') == ref-readback-Kyj1R65B ]]
    docker rm -f "$id" >/dev/null
   fi
  fi
 done
 echo cleanup_verified
 docker ps -a --format '{{.Names}}';docker network ls --format '{{.Name}}'
 exit "$status"
}
trap cleanup EXIT
[[ $(docker ps -a --format '{{.Names}}'|sort) == $(printf '%s\n' buildx_buildkit_warp-multiarch0 urnetwork-local-pg urnetwork-local-redis|sort) ]]
[[ $(docker network ls --format '{{.Name}}'|sort) == $(printf '%s\n' bridge host none urnetwork-local|sort) ]]
for arm in noop snapshot negative; do
 case $arm in
 noop) image=sha256:ba80ff30743970e4f496237ca8beb30dadcc9fcb4472a0d999bb9a66eb843cd8;commit=33420e8d987d50b74818e40cc0d1c5d589521309;;
 snapshot) image=sha256:102a8cfcdcdfa1971e9dcc169aa4a523fb0369743c9d7e64713927afc755d65c;commit=0ecf8f90ccf1516f807b20f39f3e5f55d22f4b0c;;
 negative) image=sha256:c675a2158dc3a3a0238a4421c8486d51051e09b40698c1fc92aff843150c355c;commit=e445ffb75fc3305df6c1ccd5d99e768e45e1142e;;
 esac
 docker image inspect "$image" > "$root/$arm-image-inspect.json"
 [[ $(jq -r '.[0].Id' "$root/$arm-image-inspect.json") == "$image" ]]
 echo "command=docker run --rm --pull=never --name=ref-readback-Kyj1R65B-$arm --label=urnetwork.fixture.owner=ref-readback-Kyj1R65B --network=none --read-only --user=65532:65532 --cap-drop=ALL --security-opt=no-new-privileges:true --memory=128m --memory-swap=128m --pids-limit=32 --cpuset-cpus=20,22 --env=GOTOOLCHAIN=local --entrypoint=/bin/sh $image -c '/usr/bin/sha256sum /opt/urnetwork/bin/sim-latency && /usr/local/go/bin/go version -m /opt/urnetwork/bin/sim-latency'"
 timeout --signal=TERM --kill-after=5s 15s docker run --rm --pull=never --name="ref-readback-Kyj1R65B-$arm" --label=urnetwork.fixture.owner=ref-readback-Kyj1R65B --cidfile="$root/$arm.cid" --network=none --read-only --user=65532:65532 --cap-drop=ALL --security-opt=no-new-privileges:true --memory=128m --memory-swap=128m --pids-limit=32 --cpuset-cpus=20,22 --env=GOTOOLCHAIN=local --entrypoint=/bin/sh "$image" -c '/usr/bin/sha256sum /opt/urnetwork/bin/sim-latency && /usr/local/go/bin/go version -m /opt/urnetwork/bin/sim-latency' > "$root/$arm.stdout" 2> "$root/$arm.stderr"
 grep -F '/opt/urnetwork/bin/sim-latency: go1.27.3' "$root/$arm.stdout"
 grep -F "vcs.revision=$commit" "$root/$arm.stdout"
 grep -F 'vcs.modified=false' "$root/$arm.stdout"
 head -n 1 "$root/$arm.stdout"
 if [[ $arm == negative ]]; then grep -F 'fc3709dc5dcd2448f903902d801d0e5dff6ce0274142b012fd5c029892dc9b83  /opt/urnetwork/bin/sim-latency' "$root/$arm.stdout";fi
 sha256sum "$root/$arm.stdout" "$root/$arm.stderr" "$root/$arm-image-inspect.json"
done
