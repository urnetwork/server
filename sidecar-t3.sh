#!/usr/bin/env bash
# §22.4's T3 (TEST-M1-08): server hosting sidecar's engines on the private test stores, which
# sidecar/test.sh runs after connect in RUN-ALL (D-TEST-1). sidecart3's doc says what T3 is.
#
#   sidecar-t3.sh suites    each merge (QD-374): every store suite on its real store, sidecart3's checks
#                           of T3 itself, and, when lock.yml's connect pin moved since the last run here,
#                           the T3 rows at once (Q-TEST-1: a connect pin change reruns both tiers; T2's
#                           run every merge in sidecar/test.sh)
#   sidecar-t3.sh nightly   the suites, then every FS row registered on T3, under §22.8's rules with the
#                           100 nightly explorer seeds, then §22.7's scenarios and CC3's leak self-test, then
#                           T3's cluster faults on the 4-host file-cluster
#                           layouts (server/local/file-cluster-t3/t3.sh up; TEST-M2-07), which kill and start
#                           hosts and so run only here, and say "not run" when the layouts do not answer, then
#                           §22.8's disruption rows (TEST-M2-15), which use real time and real processes
#
# Everything builds with -tags sidecar_testhooks,sidecar_pending, so the hooks and the pending migration
# groups are in, and runs with -count=1 and WARP_TEST_ENV_FAIL_FAST=1: server's TestEnv reruns a failed
# attempt 4 times otherwise, and a failed attempt must fail the run (R-TEST-9). Nothing is retried, and a
# phase that fails ends the run. The tests that need the local stack's MinIO (sidecar/local, Docker) carry
# sidecar_localstack: nightly runs them and fails when the stack is down; suites runs them when the stack
# answers and says "not run" otherwise, which is never a pass.
#
# The stores are the private ones test-env.sh attests, and it is sourced here, so its guards hold: no
# dial outside the attested list, never port 5432, 6432 or 6379-6427. This takes no suite lock, as
# sidecar/test.sh takes none: tests/run-all.sh holds the network-intensive lock around both.
#
# GO names the go command (as in GO=lowgo), TEST_TIMEOUT go test's -timeout, SIDECAR_T3_STATE_DIR where the
# last connect pin run is kept (a lost one only reruns the rows).
#
# exit: 0 every phase that ran passed; the failing phase's status otherwise; 64 usage.

set -u

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
GO=${GO:-go}
TEST_TIMEOUT=${TEST_TIMEOUT:-30m}
state_dir=${SIDECAR_T3_STATE_DIR:-${TMPDIR:-/tmp}/sidecar-t3-state}
export WARP_TEST_ENV_FAIL_FAST=1
tags=sidecar_testhooks,sidecar_pending
minio_health=http://127.0.0.1:59000/minio/health/cluster
# the first host of T3's meta-1 layout (local/file-cluster-t3/clusters.yml)
layout_health=http://127.0.0.1:52101/minio/health/cluster

cadence=${1:-}
case $cadence in
  suites|nightly) ;;
  *)
    echo "usage: sidecar-t3.sh suites|nightly" >&2
    exit 64
    ;;
esac

say() {
  printf '[sidecar-t3.sh] %s\n' "$*"
}

# shellcheck source=test-env.sh
source "$here/test-env.sh" || exit $?

# One go test in server's tree: a directory, then go test's arguments. A failure ends the run.
go_t3() {
  local directory=$1 status
  shift
  say "go test $directory $*"
  (cd "$here" && "$GO" test -count=1 -timeout "$TEST_TIMEOUT" "$@" "$directory")
  status=$?
  if [ "$status" -ne 0 ]; then
    say "$directory failed with status $status; nothing is retried, so the run fails"
    exit "$status"
  fi
}

stack_up() {
  curl -sf -o /dev/null --max-time 5 "$minio_health"
}

not_run=""
skip() {
  say "not run, which is no pass: $*"
  not_run="${not_run:+$not_run; }$*"
}

# The store suites on their real stores (sidecart3.Suites names each test), and T3's own checks.
suites() {
  go_t3 ./sidecart3 -tags "$tags" -run '^(TestEveryStoreSuiteRunsOnT3|TestSuiteProblemsFailAFakeWithNoSuite|TestSidecarT3RunsEverySuiteTest|TestFailFastLeavesNoRerun|TestCrossslotKeysSpreadOverTheMasters|TestCrossslotCallsKeepEachShapeOnce|TestRunCrossslotRefusesOneNode|TestADialOutsideTheAttestedListIsRefused|TestInlinePathFailsAPlantedPostgresRead|TestInlinePathFailsOneRedisCallOverItsRow|TestInlinePathReadsAnUnsetLimitAsMissing|TestEveryPathRowResolvesOrReadsMissing|TestCapturePartReaderMakesAPartOnlyWhenTheScanReachesIt|TestCapturePartReaderEndsWithAPartsError|TestCapturePartReaderClosedPartWayEndsItsProducer)$'
  go_t3 ./sidecarhost -tags "$tags" -run '^(TestRedisConformsOnTheTestStore|TestCountingRedisConformsOnTheTestStore)$'
  go_t3 ./model -tags "$tags" -run '^(TestSidecarPushStorePassesItsSuite|TestSocketStorePassesItsSuite|TestMxEdgeStorePassesTheSuite|TestSmtpEdgeStorePassesTheSuite|TestConfigLogSequencerPassesTheStoreSuite)$'
  go_t3 ./dnshost -tags "$tags" -run '^(TestStoreRunsTheDnsStoreSuite|TestRestoreStoreRunsItsSuite)$'
  if [ "$cadence" = nightly ] || stack_up; then
    go_t3 . -tags "$tags,sidecar_localstack" -run '^TestFileClustersConformOnTheLocalStack$'
    go_t3 ./model -tags "$tags,sidecar_localstack" -run '^TestConfigLogSequencerPassesTheStoreSuiteOnMinio$'
    go_t3 ./gkm -tags "$tags,sidecar_localstack" -run '^(TestGkmStoreSuiteOnTheLocalStack|TestGkmRoutesOnTheLocalStack)$'
    go_t3 ./sidecart3 -tags "$tags,sidecar_localstack" -run '^TestT3'
  else
    skip "blobsuite on FileClusters, the config log on MinIO, gkm's store suite and routes and T3's Nets: the local stack's MinIO does not answer (sidecar/local/stack.sh up)"
  fi
}

# Every FS row registered on T3, with the nightly seeds.
rows() {
  go_t3 ./sidecart3 -tags "$tags,sidecar_localstack" -run '^TestT3Rows$'
}

layout_up() {
  curl -sf -o /dev/null --max-time 5 "$layout_health"
}

# §22.7's scenarios on T3 (TEST-M2-11), and CC3's self-test under the leak leg's tag (TEST-M2-09).
scenarios() {
  go_t3 ./sidecart3 -tags "$tags,sidecar_localstack" -run '^TestScenario'
  go_t3 ./sidecart3 -tags "$tags,sidecar_localstack,sidecar_leaktest" -run '^TestT3EveryStoreClassCatchesItsLeak$'
}

# QuorumLoss, DriveMiss, HostDown and HostUp on T3's 4-host layouts, which take hosts down for real.
cluster_faults() {
  if layout_up; then
    go_t3 ./sidecart3 -tags "$tags,sidecar_filecluster_t3" -run '^(TestT3LayoutTakesRequestsOnAFakeClock|TestT3QuorumLossGives503AndAnUnknownOutcome|TestT3DriveMissLeavesTheObjectReadableUntilASecondHostGoesDown|TestT3AHostReturningHeals)$'
  else
    skip "T3's cluster faults: the 4-host file-cluster layouts do not answer (local/file-cluster-t3/t3.sh up)"
  fi
}

# §22.8's disruption rows beyond the schedules (TEST-M2-15; sidecart3/disruption.go): a resident killed mid-stream, a
# Redis master's restart under an evs world and a Postgres restart mid-request, carried out for real on the private
# stores, and an lb drain through nginx from warp's directives (local/sidecar-lb-fixture). They use real time, so they
# run here and never at merge.
disruptions() {
  go_t3 ./sidecart3 -tags "$tags,sidecar_localstack,sidecar_disruption" -run '^(TestT3ResidentKilledMidStreamResetsTransferFailed|TestT3ResidentMigrationKeepsAStreamWholeOrResetsIt|TestT3ResidentKilledMidCallRetriesWithOneEffect|TestT3ResidentKilledUnderACallItsRetryJoinsTheRun|TestT3RedisMasterRestartLosesOnlyRebuildableState|TestT3PostgresRestartMidRequest)$'
  if command -v "${SIDECAR_LB_NGINX:-nginx}" >/dev/null 2>&1; then
    go_t3 ./sidecart3 -tags "$tags,sidecar_disruption" -run '^(TestT3LbDrainCutsAStreamedPutWith502|TestT3LbDrainRetriesABufferedPutOnTheSibling)$'
  else
    skip "the lb drain rows: no nginx (SIDECAR_LB_NGINX or nginx on PATH)"
  fi
}

connect_pin() {
  awk '/path: connect$/ { found = 1; next } found && /ref:/ { print $2; exit }' "$here/local/source-graph/lock.yml"
}

suites
if [ "$cadence" = nightly ]; then
  rows
  scenarios
  cluster_faults
  disruptions
else
  pin=$(connect_pin)
  last_pin=$(cat "$state_dir/connect-pin" 2>/dev/null || true)
  if [ -z "$pin" ]; then
    say "lock.yml names no connect pin, so the rows run as for a changed one"
  fi
  if [ -z "$pin" ] || [ "$pin" != "$last_pin" ]; then
    if stack_up; then
      say "connect's pin is ${pin:-unknown}, last run at ${last_pin:-none}: T3's rows run now (Q-TEST-1)"
      rows
      mkdir -p "$state_dir" && printf '%s\n' "$pin" >"$state_dir/connect-pin"
    else
      skip "T3's rows after connect's pin moved to ${pin:-unknown}: the local stack's MinIO does not answer"
    fi
  fi
fi

if [ -n "$not_run" ]; then
  say "cadence $cadence: passed what ran; not run: $not_run"
else
  say "cadence $cadence: every phase ok"
fi
exit 0
