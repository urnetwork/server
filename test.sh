#!/usr/bin/env bash

set -o pipefail

script_path="${BASH_SOURCE[0]}"
script_dir="${script_path%/*}"
[[ "$script_dir" != "$script_path" ]] || script_dir="."
script_dir="$(cd -- "$script_dir" >/dev/null 2>&1 && pwd)" || exit $?
cd "$script_dir" || exit $?
workspace_root="${URNETWORK_ROOT:-${WARP_HOME:-${script_dir%/*}}}"
network_test_gate="$workspace_root/tests/network-intensive-suite-lock.sh"
if [[ ! -x "$network_test_gate" ]]; then
    echo "server test suite gate is missing or not executable: $network_test_gate" >&2
    exit 127
fi
if [[ "${URNETWORK_NETWORK_TEST_LOCK_HELD:-}" != 1 ]]; then
    exec "$network_test_gate" run-all run-all-server -- "$script_dir/test.sh" "$@"
fi
if ! "$network_test_gate" --verify-held run-all; then
    echo "server test suite inherited an invalid network-intensive lock" >&2
    exit 70
fi
source "$script_dir/test-env.sh" || exit $?

# Dashboard expressions use the official evaluator against synthetic series.
# An explicit tool path keeps frozen/offline campaigns out of the installer.
URNETWORK_PROMTOOL="$(bash "$script_dir/grafana/promtool.sh")" || exit $?
export URNETWORK_PROMTOOL

# Propagates a real output-filter failure before the upstream test status. A
# filter that closes early otherwise turns the test process into a misleading
# SIGPIPE exit and hides the component that actually failed.
test_pipeline_status() {
    local test_status="$1"
    local filter_status="$2"
    if [[ "$filter_status" != 0 ]]; then
        echo "test output filter failed with status $filter_status (upstream test status $test_status)" >&2
        return "$filter_status"
    fi
    return "$test_status"
}

# Mandatory admission regression, even when a later package run is filtered.
# Real PostgreSQL+Redis and independent processes must prove the same-network
# shared-grant path does not regain financial row queues or a local payer gate.
# No retries or skip-on-short mode: a missing fixture fails the release gate.
WARP_TEST_ENV_FAIL_FAST=1 go test -p=1 -race -count=1 -timeout=5m ./model ./taskworker/work \
    -run '^(TestRedisAdmission.*|TestAsyncDebit.*|TestTransferDebitTaskPartitionsAndBoundedContinuation|TestSettlementParticipantReadsDoNotHoldSharedGrant|TestRedisSettlementCompletesWithSharedFinancialRowsHeld|TestContractCreationSameNetworkLargeNContentionFree|TestContractSettlementSameNetworkLargeNContentionFree|TestPrivateProviderCreationDoesNotQueueOnSharedFinancialRows)$' || exit $?

# The proxy integration tests (./proxy) drive real-time wireguard/gvisor packet
# paths and real outbound TLS. Like the connect packet-translation tests, the
# race detector's scheduling overhead slows that real-time delivery enough to
# stall them, and with the main loop's `-timeout 0` a stall hangs the whole
# integration run indefinitely. Run the proxy package on its own, up front in a
# fresh process, the way proxy/test.sh runs it: no -race, and a finite timeout
# so a stall fails the step instead of hanging forever. It is skipped in the
# -race loop below.
proxy_dir="./proxy"
if [[ -d $proxy_dir ]]; then
    pushd $proxy_dir
    match="/${PWD##*/}/\\S*\.go\|^\\S*_test.go"
    go test -timeout 30m -v "$@" | grep --binary-files=text --line-buffered --color=always -e "^" -e "$match"
    pipeline_status=("${PIPESTATUS[@]}")
    test_pipeline_status "${pipeline_status[0]}" "${pipeline_status[1]}" || exit $?
    popd
fi

# PERFVAR separates production-shaped, wall-clock-sensitive DB correctness
# from race-instrumented ownership and concurrency checks. Running its regional,
# loss, outage, and throughput fixtures under -race changes their modeled timing
# enough to create false route and workload timeouts. Run the complete serial
# correctness tier first, then the package's documented short race tier.
perfvar_dir="./connect/perfvar"
if [[ -d $perfvar_dir ]]; then
    pushd $perfvar_dir
    match="/${PWD##*/}/\S*\.go\|^\S*_test.go"
    go test -timeout 0 -p=1 -parallel=1 -v "$@" | grep --binary-files=text --line-buffered --color=always -e "^" -e "$match"
    pipeline_status=("${PIPESTATUS[@]}")
    test_pipeline_status "${pipeline_status[0]}" "${pipeline_status[1]}" || exit $?
    GORACE="log_path=profile/race.out halt_on_error=1" go test -timeout 30m -p=1 -parallel=1 -short -v -race -cpuprofile profile/cpu -memprofile profile/memory "$@" | grep --binary-files=text --line-buffered --color=always -e "^" -e "$match"
    pipeline_status=("${PIPESTATUS[@]}")
    test_pipeline_status "${pipeline_status[0]}" "${pipeline_status[1]}" || exit $?
    popd
fi

# Evaluator and baseline outputs are immutable evidence, not Go packages. Some
# retained campaigns contain standalone *_test.go policy fixtures and
# root-owned trees; verify the preserved baseline through its manifest, then
# keep all evidence out of source test discovery.
baseline_dir="./connect/sim-latency/baseline"
if [[ -d $baseline_dir ]]; then
    "$baseline_dir/verify.sh" || exit $?
fi

# The model package's TestStNativeFeeSettlementPublic* roots settle the
# native-fee fixtures and the mainnet.test verifier that only the sn release
# gate builds (sn/scripts/build-mainnet-exports.sh). The gate exports them as
# URNETWORK_NATIVE_FEE_SETTLEMENT_* and runs these roots in its server-db phase
# (sn/scripts/test-release-1.0-local.sh). By design a missing asset fails them:
# an unexecuted fixture is never a skip (model/st_native_fee_settlement_public_test.go).
# An ordinary local or CI run sets none of the gate's variables and cannot
# execute them, so it leaves them out here instead of reporting four certain
# failures. With any of the variables set, even empty, the run is the gate or
# reproduces it: the roots run, and a missing or partial export fails as
# designed. go test keeps only the last -skip, so a caller's own -skip
# replaces this one.
model_skip_args=()
if [[ -z "${URNETWORK_NATIVE_FEE_SETTLEMENT_FIXTURE_DIRECTORY+set}${URNETWORK_NATIVE_FEE_SETTLEMENT_VERIFIER+set}${URNETWORK_NATIVE_FEE_SETTLEMENT_VERIFIER_SHA256+set}" ]]; then
    model_skip_args=(-skip TestStNativeFeeSettlementPublic)
fi

test_directories="$(./test-dirs.sh)" || exit $?
while IFS= read -r d; do
    [[ -n "$d" ]] || continue
    if [[ $d == $proxy_dir || $d == $perfvar_dir ]]; then
        # run separately above with each integration package's timing contract
        continue
    fi
    package_args=()
    if [[ $d == ./model ]]; then
        package_args=("${model_skip_args[@]}")
    fi
    # if [[ $1 == "" || $1 == `basename $d` ]]; then
        pushd "$d"
        # highlight source files in this dir
        match="/${PWD##*/}/\\S*\.go\|^\\S*_test.go"
        # go test -v "$@" | grep --color=always -e "^" -e "$match"
        GORACE="log_path=profile/race.out halt_on_error=1" go test -timeout 0 -v -race -cpuprofile profile/cpu -memprofile profile/memory "${package_args[@]}" "$@" | grep --binary-files=text --line-buffered --color=always -e "^" -e "$match"
        pipeline_status=("${PIPESTATUS[@]}")
        test_pipeline_status "${pipeline_status[0]}" "${pipeline_status[1]}" || exit $?
        popd
    # fi
done <<< "$test_directories"
# stdbuf -i0 -o0 -e0

# ./test.sh -run 'pattern'
# ./test.sh -short
