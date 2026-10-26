#!/bin/bash
set -Eeuo pipefail
root=/home/by/urnetwork/temp/snow-sql-fixed-20261026.zbvTnwGA
export PATH=/home/by/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.27.3.linux-amd64/bin:$PATH GOTOOLCHAIN=local GOMAXPROCS=2 GOFLAGS=-p=2 GOPROXY=off GOSUMDB=off GOMODCACHE=/home/by/go/pkg/mod GOCACHE=/home/by/.cache/go-build GOWORK=$root/manager.work COMPETITION_WINNER_REVIEW_SQL_TEST=true
for key in ${!SIM_LATENCY_@}; do unset "$key"; done
exec 9<>/home/by/urnetwork/temp/production-frontier-20261024.63AtMXS5/scoring.lock
flock -n 9
source /home/by/urnetwork/temp/main-targeted-2f46-corrected-20261026.3Nd6V666/fixture-provisioned/services/environment.sh
unset WARP_TEST_ENV_ATTESTED_ADDRS WARP_TEST_ENV_LAUNCHER_VAULT_HOME
[[ $(docker image inspect postgres:18 --format '{{.Id}}') == sha256:8d6ed4b5a1f44557313cab42a8951c53d1b86ccc899266bcee2ee1ee84076c14 ]]
cd "$root/server"
[[ $(git rev-parse HEAD) == e82b4ea3cc08615baa67eabdc8da5ef17ffcedbf ]]
[[ -z $(git status --porcelain) ]]
go version
sha256sum "$GOWORK" go.mod go.sum competition_snow_winner_review_migration*.go competition_winner_review_migration_test.go > "$root/source-receipt.sha256"
for repo in connect glog goidenticons gvisor proxy sdk sn userwireguard warp; do printf '%s %s ' "$repo" "$(readlink "$root/$repo")"; git -C "$root/$repo" rev-parse HEAD; done > "$root/dependencies.txt"
timeout --signal=TERM --kill-after=45s 900s go test -race -c -o "$root/sql.test" . > "$root/compile.stdout" 2> "$root/compile.stderr"
sha256sum "$root/sql.test"
for n in 1 2 3; do
 start=$SECONDS
 set +e
 timeout --signal=TERM --kill-after=45s 600s "$root/sql.test" -test.run '^TestCompetitionSnowReview' -test.count=1 -test.timeout=5m -test.v > "$root/pass-$n.stdout" 2> "$root/pass-$n.stderr"
 status=$?
 set -e
 echo pass=$n exit=$status seconds=$((SECONDS-start))
 sha256sum "$root/pass-$n.stdout" "$root/pass-$n.stderr"
 ((status==0)) || { cat "$root/pass-$n.stdout"; cat "$root/pass-$n.stderr"; exit "$status"; }
 [[ $(grep -c '^--- PASS:' "$root/pass-$n.stdout") == 3 ]]
 ! grep -q '^--- SKIP:' "$root/pass-$n.stdout"
done
echo PASS_SQLSnow3_three
