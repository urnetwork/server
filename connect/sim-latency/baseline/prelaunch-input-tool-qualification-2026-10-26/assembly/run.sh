#!/bin/bash
set -Eeuo pipefail
root=/home/by/urnetwork/temp/assembly-qualification-20261026.11sINLgK
export PATH=/home/by/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.27.3.linux-amd64/bin:$PATH
export GOTOOLCHAIN=local GOMAXPROCS=2 GOFLAGS=-p=2 GOWORK=$root/manager.work GOPROXY=off
for key in ${!SIM_LATENCY_@}; do unset "$key"; done
export SIM_LATENCY_QUALIFY_ASSEMBLY=1
source /home/by/urnetwork/temp/warm-readers-negative-qualification-20261025.15cNHvs8/services/environment.sh
unset WARP_TEST_ENV_ATTESTED_ADDRS WARP_TEST_ENV_LAUNCHER_VAULT_HOME
exec 9<>/home/by/urnetwork/temp/production-frontier-20261024.63AtMXS5/scoring.lock
flock -n 9
cd "$root/server"
[[ $(git rev-parse HEAD) == 30f340a2779699225ab40ec7f3c5d978525276d2 ]]
go version
sha256sum connect/sim-latency/calibration_input_assembly{,_qualification}_test.go "$GOWORK"
for n in 1 2 3; do
 start=$SECONDS
 echo "command=go test -race -count=1 -timeout=5m -v ./connect/sim-latency -run ^TestAssemblyQualification"
 set +e
 timeout --signal=TERM --kill-after=45s 600s go test -race -count=1 -timeout=5m -v ./connect/sim-latency -run '^TestAssemblyQualification' > "$root/pass-$n.stdout" 2> "$root/pass-$n.stderr"
 status=$?;set -e
 echo pass=$n exit=$status duration_seconds=$((SECONDS-start))
 sha256sum "$root/pass-$n.stdout" "$root/pass-$n.stderr"
 grep -c '^--- PASS:' "$root/pass-$n.stdout" || true
 ((status==0)) || { tail -n 30 "$root/pass-$n.stdout";cat "$root/pass-$n.stderr";exit "$status"; }
 ! grep -q '^--- SKIP:' "$root/pass-$n.stdout"
done
