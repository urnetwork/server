#!/usr/bin/env bash
# §22.4's T3 (TEST-M1-08): server hosting sidecar's engines on the private test stores, which
# sidecar/test.sh runs after connect in RUN-ALL (D-TEST-1). sidecart3's doc says what T3 is.
#
#   sidecar-t3.sh suites    each merge (QD-374): every store suite on its real store, sidecart3's checks
#                           of T3 itself, and, when lock.yml's connect pin moved since the last run here,
#                           the T3 rows at once (Q-TEST-1: a connect pin change reruns both tiers; T2's
#                           run every merge in sidecar/test.sh)
#   sidecar-t3.sh nightly   the suites, then every FS row registered on T3, under §22.8's rules with the
#                           100 nightly explorer seeds
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
  go_t3 ./sidecart3 -tags "$tags" -run '^(TestEveryStoreSuiteRunsOnT3|TestSuiteProblemsFailAFakeWithNoSuite|TestSidecarT3RunsEverySuiteTest|TestFailFastLeavesNoRerun|TestCrossslotKeysSpreadOverTheMasters|TestCrossslotCallsKeepEachShapeOnce|TestRunCrossslotRefusesOneNode|TestADialOutsideTheAttestedListIsRefused)$'
  go_t3 ./sidecarhost -tags "$tags" -run '^(TestRedisConformsOnTheTestStore|TestCountingRedisConformsOnTheTestStore)$'
  go_t3 ./model -tags "$tags" -run '^(TestSidecarPushStorePassesItsSuite|TestSocketStorePassesItsSuite|TestMxEdgeStorePassesTheSuite|TestConfigLogSequencerPassesTheStoreSuite)$'
  go_t3 ./dnshost -tags "$tags" -run '^TestStoreRunsTheDnsStoreSuite$'
  if [ "$cadence" = nightly ] || stack_up; then
    go_t3 . -tags "$tags,sidecar_localstack" -run '^TestFileClustersConformOnTheLocalStack$'
    go_t3 ./model -tags "$tags,sidecar_localstack" -run '^TestConfigLogSequencerPassesTheStoreSuiteOnMinio$'
    go_t3 ./sidecart3 -tags "$tags,sidecar_localstack" -run '^TestT3'
  else
    skip "blobsuite on FileClusters, the config log on MinIO and T3's Nets: the local stack's MinIO does not answer (sidecar/local/stack.sh up)"
  fi
}

# Every FS row registered on T3, with the nightly seeds.
rows() {
  go_t3 ./sidecart3 -tags "$tags,sidecar_localstack" -run '^TestT3Rows$'
}

connect_pin() {
  awk '/path: connect$/ { found = 1; next } found && /ref:/ { print $2; exit }' "$here/local/source-graph/lock.yml"
}

suites
if [ "$cadence" = nightly ]; then
  rows
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
