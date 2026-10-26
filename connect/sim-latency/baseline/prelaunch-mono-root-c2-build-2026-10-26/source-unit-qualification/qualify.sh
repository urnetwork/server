#!/bin/bash
# PREPARED ONLY. Parent must explicitly authorize after live analysis/C2 build.
set -Eeuo pipefail
[[ ${AUTHORIZE_NEXT_IDLE_UNITS:-} == yes ]]
root=/home/by/urnetwork/temp/next-idle-plan-20261026.aEKfrDr8
export PATH=/home/by/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.27.3.linux-amd64/bin:$PATH GOTOOLCHAIN=local GOMAXPROCS=2 GOFLAGS=-p=2 GOPROXY=off GOSUMDB=off GOMODCACHE=/home/by/go/pkg/mod GOCACHE=/home/by/.cache/go-build
for key in ${!SIM_LATENCY_@}; do unset "$key"; done
[[ $(id -u) == 0 ]]
exec 9<>/home/by/urnetwork/temp/production-frontier-20261024.63AtMXS5/scoring.lock
flock -n 9
source /home/by/urnetwork/temp/main-targeted-2f46-corrected-20261026.3Nd6V666/fixture-provisioned/services/environment.sh
unset WARP_TEST_ENV_ATTESTED_ADDRS WARP_TEST_ENV_LAUNCHER_VAULT_HOME
export GIT_CONFIG_GLOBAL=/home/by/urnetwork/temp/main-targeted-2f46-corrected-20261026.3Nd6V666/gitconfig
go version
git clone --quiet --shared --no-checkout /home/by/urnetwork/temp/mono-root-c2-release-20261026.PUHdHLML/source/server "$root/c2-server"
git -C "$root/c2-server" checkout --quiet --detach a9e8e1a710b24c749e6499a0606938252c83e136
cp /home/by/urnetwork/temp/assembly-qualification-20261026.11sINLgK/manager.work "$root/c2.work"
export GOWORK=$root/c2.work
go work edit -dropuse=/home/by/urnetwork/temp/assembly-qualification-20261026.11sINLgK/server -use="$root/c2-server"
cd "$root/c2-server"
timeout --signal=TERM --kill-after=45s 900s go test -race -c -o "$root/c2.test" ./connect/sim-latency > "$root/c2.compile.stdout" 2> "$root/c2.compile.stderr"
c2regex='^(TestEvaluatorMonoSourceLockAcceptsDirectCandidateInEveryRepository|TestEvaluatorSourceLockRejectsUnknownCandidatePathBase|TestEvaluatorSourceLockRequiresCompleteRepositorySet|TestEvaluatorSourceLockRejectsUnexpectedRepositories|TestReadWinnerSignificanceRequiresEligibleR9Score|TestEvaluatorSourceLockAcceptsOnlyOneServerCandidateCommit)$'
for n in 1 2 3; do
 timeout --signal=TERM --kill-after=45s 600s "$root/c2.test" -test.run "$c2regex" -test.count=1 -test.timeout=5m -test.v > "$root/c2-$n.stdout" 2> "$root/c2-$n.stderr"
 [[ $(grep -c '^--- PASS:' "$root/c2-$n.stdout") == 6 ]]
 ! grep -q '^--- SKIP:' "$root/c2-$n.stdout"
done
git clone --quiet --shared --no-checkout /home/by/urnetwork/server "$root/control-server"
git -C "$root/control-server" checkout --quiet --detach 21f371d180bc62a5f11a94ee23ccb31c23204314
cp /home/by/urnetwork/temp/calibration-path-fix-20261026.ne1Lrvte/clean-manager.work "$root/control.work"
export GOWORK=$root/control.work
go work edit -dropuse=/home/by/urnetwork/temp/calibration-path-fix-20261026.ne1Lrvte/clean-server -use="$root/control-server"
cd "$root/control-server"
git clone --quiet --shared --no-checkout /home/by/urnetwork/server "$root/draft-donor"
git -C "$root/draft-donor" sparse-checkout set connect/sim-latency/launch
git -C "$root/draft-donor" checkout --quiet --detach 87e1489b6176f14634a59fc8e0422966c0260658
for input in "$root/draft-donor"/connect/sim-latency/launch/held-out-*.go.txt; do
 name=$(basename "$input" .go.txt)
 cp "$input" "connect/sim-latency/${name//-/_}_draft_test.go"
done
git -C "$root/draft-donor" checkout --quiet --detach 7bec48b779b4e6c40d1e58102a39e8c638633012
for input in "$root/draft-donor"/connect/sim-latency/launch/review-qualification*.go.txt; do
 name=$(basename "$input" .go.txt)
 cp "$input" "connect/sim-latency/${name//-/_}_draft_test.go"
done
sha256sum connect/sim-latency/held_out_*_draft_test.go connect/sim-latency/review_qualification*_draft_test.go "$GOWORK" > "$root/control-source.sha256"
timeout --signal=TERM --kill-after=45s 900s go test -race -c -o "$root/control.test" ./connect/sim-latency > "$root/control.compile.stdout" 2> "$root/control.compile.stderr"
sha256sum "$root/control.test"
for selection in heldout review native; do
 case $selection in heldout) pattern='^TestHeldOut'; count=87;; review) pattern='^TestReviewQualification'; count=16;; native) pattern='^TestCalibrationProtocol'; count=9;; esac
 "$root/control.test" -test.list "$pattern" > "$root/$selection.names"
 [[ $(grep -c '^Test' "$root/$selection.names") == $count ]]
 for n in 1 2 3; do
  start=$SECONDS
  set +e
  timeout --signal=TERM --kill-after=45s 1230s "$root/control.test" -test.run "$pattern" -test.count=1 -test.timeout=20m -test.v > "$root/$selection-$n.stdout" 2> "$root/$selection-$n.stderr"
  status=$?
  set -e
  echo selection=$selection pass=$n exit=$status seconds=$((SECONDS-start))
  sha256sum "$root/$selection-$n.stdout" "$root/$selection-$n.stderr"
  ((status==0)) || { tail -50 "$root/$selection-$n.stdout"; cat "$root/$selection-$n.stderr"; exit "$status"; }
  [[ $(grep -c '^--- PASS:' "$root/$selection-$n.stdout") == $count ]]
  ! grep -q '^--- SKIP:' "$root/$selection-$n.stdout"
 done
done
echo PASS_C2_and_amended_heldout_review_native
# Broader current-main four-package scope is a separately selected invocation:
# reuse exact pinned graph and names in prior main-targeted receipt; add model
# TestWinnerProviderPolicy*, TestWinnerReviewPolicy*, TestCompetitionAutomationScope*.
# Its fixture is freshly attested with release_gate_services_start NEWROOT ROOT LOCK.
# No broader package selection runs automatically from this priority script.
