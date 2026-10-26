#!/bin/bash
set -Eeuo pipefail
root=/home/by/urnetwork/temp/global-identity-runtime-20261026.2vFN6RfP
export PATH=/home/by/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.27.3.linux-amd64/bin:$PATH GOTOOLCHAIN=local GOMAXPROCS=2 GOFLAGS=-p=2 GOPROXY=off GOSUMDB=off GOMODCACHE=/home/by/go/pkg/mod GOCACHE=/home/by/.cache/go-build GOWORK=$root/manager.work GIT_CONFIG_GLOBAL=/home/by/urnetwork/temp/main-targeted-2f46-corrected-20261026.3Nd6V666/gitconfig
for key in ${!SIM_LATENCY_@}; do unset "$key"; done
exec 9<>/home/by/urnetwork/temp/production-frontier-20261024.63AtMXS5/scoring.lock
flock -n 9
source /home/by/urnetwork/temp/main-targeted-2f46-corrected-20261026.3Nd6V666/fixture-provisioned/services/environment.sh
unset WARP_TEST_ENV_ATTESTED_ADDRS WARP_TEST_ENV_LAUNCHER_VAULT_HOME
cd "$root/server"
git diff --exit-code -- connect/sim-latency/source_git_filter_test.go connect/sim-latency/source_epoch_test.go
git rev-parse HEAD
sha256sum "$GOWORK" go.mod go.sum connect/sim-latency/source*test.go connect/sim-latency/held_out_*_draft_test.go > "$root/source-receipt.sha256"
timeout --signal=TERM --kill-after=45s 900s go test -race -c -o "$root/sim.test" ./connect/sim-latency > "$root/sim.compile.stdout" 2> "$root/sim.compile.stderr"
sha256sum "$root/sim.test"
cd connect/sim-latency
for selection in builder runtime; do
 case $selection in builder) regex='^(TestSubmissionBuilder.*|TestSourceLocalGitCannotExecuteConfiguredAttributeFilters|TestSourceGitSeparatesLocalAndTransportConfiguration|TestSourceGitReadsForeignOwnedRootAndSubdirectory|TestSourceGitScopesExecutionWithoutDroppingTransportEnvironment|TestSourceGitRejectsAmbiguousRepositoryPaths|TestSourceBuildSealsExecutableBitsFromGitTree|TestReviewedSourceChecksRealPinnedModesForStagingAndPublication|TestSourceBuildSandboxClone.*|TestSourceBuildSandboxContext.*|TestSourceGitFixtureRetainsExplicitGlobalIdentity|TestSourceFixtureGitUsesGlobalIdentity)$'; expected=19;; runtime) regex='^TestHeldOut(Runtime.*|Amendment.*|Bridge.*|OperatorPreparesCompleteNativeFiveRoundPlan|ExecutionCompletesAllJobsWithOneControlPerRound|ExecutionOriginalDeadlineCannotRestart|ExecutionDoesNotRetryInfrastructureFailure)$'; expected=39;; esac
 "$root/sim.test" -test.list "$regex" > "$root/$selection.names"
 [[ $(grep -c '^Test' "$root/$selection.names") == $expected ]]
 for n in 1 2 3; do
  start=$SECONDS
  set +e
  timeout --signal=TERM --kill-after=45s 1230s "$root/sim.test" -test.run "$regex" -test.count=1 -test.timeout=20m -test.v > "$root/$selection-$n.stdout" 2> "$root/$selection-$n.stderr"
  status=$?
  set -e
  echo selection=$selection pass=$n exit=$status seconds=$((SECONDS-start))
  sha256sum "$root/$selection-$n.stdout" "$root/$selection-$n.stderr"
  ((status==0)) || { tail -60 "$root/$selection-$n.stdout"; cat "$root/$selection-$n.stderr"; exit "$status"; }
  [[ $(grep -c '^--- PASS:' "$root/$selection-$n.stdout") == $expected ]]
  ! grep -q '^--- SKIP:' "$root/$selection-$n.stdout"
 done
done
echo PASS_builder19_runtime39_three
