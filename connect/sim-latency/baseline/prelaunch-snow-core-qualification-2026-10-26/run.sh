#!/bin/bash
set -Eeuo pipefail
root=/home/by/urnetwork/temp/snow-core-qualification-20261026.rPRAxV8K
export PATH=/home/by/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.27.3.linux-amd64/bin:$PATH GOTOOLCHAIN=local GOMAXPROCS=2 GOFLAGS=-p=2 GOPROXY=off GOSUMDB=off GOMODCACHE=/home/by/go/pkg/mod GOCACHE=/home/by/.cache/go-build GOWORK=$root/manager.work GIT_CONFIG_GLOBAL=/home/by/urnetwork/temp/main-targeted-2f46-corrected-20261026.3Nd6V666/gitconfig
for key in ${!SIM_LATENCY_@}; do unset "$key"; done
exec 9<>/home/by/urnetwork/temp/production-frontier-20261024.63AtMXS5/scoring.lock
flock -n 9
source /home/by/urnetwork/temp/main-targeted-2f46-corrected-20261026.3Nd6V666/fixture-provisioned/services/environment.sh
unset WARP_TEST_ENV_ATTESTED_ADDRS WARP_TEST_ENV_LAUNCHER_VAULT_HOME
cd "$root/server"
[[ $(git rev-parse HEAD) == 74a456bcb2ebe42fc11b233cb9517c9e262aa148 ]]
[[ -z $(git status --porcelain) ]]
go version
git rev-parse HEAD
sha256sum "$GOWORK" go.mod go.sum connect/sim-latency/*snow*.go model/*snow*.go > "$root/source-receipt.sha256"
cp /home/by/urnetwork/temp/main-targeted-2f46-corrected-20261026.3Nd6V666/dependencies-complete.txt "$root/dependencies.txt"
for selection in sim model; do
 case $selection in sim) pkg=./connect/sim-latency; regex='^(TestWinnerSnow|TestSourceTestSnow)'; expected=12;; model) pkg=./model; regex='^TestSnowWinner'; expected=5;; esac
 echo compile_selection=$selection start_utc=$(date -u +%FT%TZ)
 timeout --signal=TERM --kill-after=45s 900s go test -race -c -o "$root/$selection.test" "$pkg" > "$root/$selection.compile.stdout" 2> "$root/$selection.compile.stderr" || { cat "$root/$selection.compile.stderr"; exit 1; }
 sha256sum "$root/$selection.test"
 "$root/$selection.test" -test.list "$regex" > "$root/$selection.names"
 [[ $(grep -c '^Test' "$root/$selection.names") == $expected ]]
 for n in 1 2 3; do
  start=$SECONDS
  set +e
  (cd "$root/server/${pkg#./}"; timeout --signal=TERM --kill-after=45s 600s "$root/$selection.test" -test.run "$regex" -test.count=1 -test.timeout=5m -test.v) > "$root/$selection-$n.stdout" 2> "$root/$selection-$n.stderr"
  status=$?
  set -e
  echo selection=$selection pass=$n exit=$status seconds=$((SECONDS-start))
  sha256sum "$root/$selection-$n.stdout" "$root/$selection-$n.stderr"
  ((status==0)) || { cat "$root/$selection-$n.stdout"; cat "$root/$selection-$n.stderr"; exit "$status"; }
  [[ $(grep -c '^--- PASS:' "$root/$selection-$n.stdout") == $expected ]]
  ! grep -q '^--- SKIP:' "$root/$selection-$n.stdout"
 done
done
[[ -z $(git status --porcelain) ]]
echo PASS_snow_sim12_model5_three
