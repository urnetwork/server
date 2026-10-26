#!/bin/bash
set -Eeuo pipefail
root=/home/by/urnetwork/temp/heldout-qualification-20261026.vFI2XdtL
export PATH=/home/by/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.27.3.linux-amd64/bin:$PATH
export GOTOOLCHAIN=local GOMAXPROCS=2 GOFLAGS=-p=2 GOPROXY=off GOMODCACHE=/home/by/go/pkg/mod GOCACHE=/home/by/.cache/go-build GOWORK=$root/manager.work
for key in ${!SIM_LATENCY_@}; do unset "$key"; done
exec 9<>/home/by/urnetwork/temp/production-frontier-20261024.63AtMXS5/scoring.lock
flock -n 9
[[ $(id -u) == 0 ]]
source /home/by/urnetwork/temp/warm-readers-negative-qualification-20261025.15cNHvs8/services/environment.sh
unset WARP_TEST_ENV_ATTESTED_ADDRS WARP_TEST_ENV_LAUNCHER_VAULT_HOME
cd "$root/server"
[[ $(git rev-parse HEAD) == 30f340a2779699225ab40ec7f3c5d978525276d2 ]]
go version
sha256sum "$GOWORK" "$root"/server/connect/sim-latency/held_out_*_draft_test.go > "$root/source-sha256.txt"
for repo in connect glog goidenticons gvisor proxy sdk sn userwireguard warp; do
 printf '%s ' "$repo"; git -C /home/by/urnetwork/temp/production-20261024.bUpBTuEw/$repo rev-parse HEAD
done > "$root/dependencies.txt"
echo compile_start=$(date -u +%FT%TZ)
set +e
timeout --signal=TERM --kill-after=45s 900s go test -race -c -o "$root/heldout.test" ./connect/sim-latency > "$root/compile.stdout" 2> "$root/compile.stderr"
status=$?
set -e
echo compile_exit=$status
((status==0)) || { cat "$root/compile.stderr"; exit "$status"; }
sha256sum "$root/heldout.test"
"$root/heldout.test" -test.list '^TestHeldOut' > "$root/census.txt"
[[ $(grep -c '^TestHeldOut' "$root/census.txt") == 62 ]]
for pass in 1 2 3; do
 start=$SECONDS
 echo command="$root/heldout.test -test.run ^TestHeldOut -test.count=1 -test.timeout=15m -test.v"
 set +e
 timeout --signal=TERM --kill-after=45s 930s "$root/heldout.test" -test.run '^TestHeldOut' -test.count=1 -test.timeout=15m -test.v > "$root/pass-$pass.stdout" 2> "$root/pass-$pass.stderr"
 status=$?
 set -e
 echo pass=$pass exit=$status duration_seconds=$((SECONDS-start))
 sha256sum "$root/pass-$pass.stdout" "$root/pass-$pass.stderr"
 ((status==0)) || { tail -50 "$root/pass-$pass.stdout"; cat "$root/pass-$pass.stderr"; exit "$status"; }
 [[ $(grep -c '^--- PASS:' "$root/pass-$pass.stdout") == 62 ]]
 ! grep -q '^--- SKIP:' "$root/pass-$pass.stdout"
done
echo PASS_three_same_binary_62_tests
