#!/bin/bash
set -Eeuo pipefail
root=/home/by/urnetwork/temp/dns-unit-qualification-20261026.2y0PibAV
export PATH=/home/by/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.27.3.linux-amd64/bin:$PATH
export GOTOOLCHAIN=local GOMAXPROCS=2 GOFLAGS=-p=2 GOPROXY=off GOWORK=$root/manager.work
for key in ${!SIM_LATENCY_@}; do unset "$key"; done
exec 9<>/home/by/urnetwork/temp/production-frontier-20261024.63AtMXS5/scoring.lock
flock -n 9
cd "$root/server/dns_probe_qualification"
go version
go list -m golang.org/x/net
sha256sum main.go main_test.go
test "$(sha256sum main.go|cut -d' ' -f1)" = 1dbfd7099643b2f0f71f2273248d277296f22001246779e674129401afabe193
test "$(sha256sum main_test.go|cut -d' ' -f1)" = 1d110e7c6c1c983ac93c9d3b4d751189254b87abee925160586f55c1258c8ce3
run() {
 phase=$1;pattern=$2;expected=$3;start=$SECONDS
 echo "command=go test -race -count=1 -timeout=2m -v . -run $pattern"
 sha256sum main.go
 set +e
 timeout --signal=TERM --kill-after=15s 300s go test -race -count=1 -timeout=2m -v . -run "$pattern" > "$root/$phase.log" 2>&1
 status=$?;set -e
 echo phase=$phase exit=$status duration_seconds=$((SECONDS-start))
 sha256sum "$root/$phase.log"
 cat "$root/$phase.log"
 if [[ $expected == pass ]]; then test "$status" = 0; ! grep -q '^--- SKIP:' "$root/$phase.log";else test "$status" != 0; ! grep -q 'build failed' "$root/$phase.log";fi
}
run initial '^Test' pass
echo READY_FOR_MUTANT_ANSWER
read -r proceed
run mutant-answer '^TestNonceAnswerAcceptsUncompressedEquivalent$' fail
echo READY_FOR_MUTANT_TCP
read -r proceed
run mutant-tcp '^TestServeTcpRecordsQueryWhenResponseWriteFails$' fail
echo READY_FOR_RESTORE
read -r proceed
test "$(sha256sum main.go|cut -d' ' -f1)" = 1dbfd7099643b2f0f71f2273248d277296f22001246779e674129401afabe193
for n in 1 2 3; do run restored-$n '^Test' pass;done
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o "$root/dns-probe" .
sha256sum "$root/dns-probe"
go version -m "$root/dns-probe" > "$root/buildinfo.log"
cat "$root/buildinfo.log"
echo PASS_final_restore_and_unit_binary_build
