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
# Go's alarm covers a whole package, including every disposable database's
# migrations. Give each mandatory root its own unchanged five-minute bound so
# completed cases cannot consume a healthy successor's setup/operation budget.
run_mandatory_admission_tests() {
    local admission_pattern='^(TestRedisAdmission.*|TestAsyncDebit.*|TestTransferDebitTaskPartitionsAndBoundedContinuation|TestSettlementParticipantReadsDoNotHoldSharedGrant|TestRedisSettlementCompletesWithSharedFinancialRowsHeld|TestContractCreationSameNetworkLargeNContentionFree|TestContractSettlementSameNetworkLargeNContentionFree|TestPrivateProviderCreationDoesNotQueueOnSharedFinancialRows)$'
    local admission_package admission_listing admission_status admission_test previous_test
    local -a admission_tests
    for admission_package in ./model ./taskworker/work; do
        admission_listing="$(WARP_TEST_ENV_FAIL_FAST=1 go test -p=1 -race -count=1 -timeout=5m "$admission_package" -list "$admission_pattern")"
        admission_status=$?
        printf '%s\n' "$admission_listing"
        [[ "$admission_status" -eq 0 ]] || return "$admission_status"
        admission_tests=()
        while IFS= read -r admission_test; do
            [[ "$admission_test" == Test* ]] || continue
            if [[ ! "$admission_test" =~ ^Test[[:alnum:]_]+$ || ! "$admission_test" =~ $admission_pattern ]]; then
                echo "invalid mandatory admission test census for $admission_package: $admission_test" >&2
                return 65
            fi
            for previous_test in "${admission_tests[@]}"; do
                if [[ "$admission_test" == "$previous_test" ]]; then
                    echo "duplicate mandatory admission test for $admission_package: $admission_test" >&2
                    return 65
                fi
            done
            admission_tests+=("$admission_test")
        done <<< "$admission_listing"
        if [[ "${#admission_tests[@]}" -eq 0 ]]; then
            echo "empty mandatory admission test census for $admission_package" >&2
            return 65
        fi
        for admission_test in "${admission_tests[@]}"; do
            printf 'server admission test: %s %s\n' "$admission_package" "$admission_test"
            WARP_TEST_ENV_FAIL_FAST=1 go test -p=1 -race -count=1 -timeout=5m "$admission_package" -run "^${admission_test}$" || return $?
        done
    done
}
run_mandatory_admission_tests || exit $?

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

# The six public native-fee roots, four in ./model and two in ./controller,
# settle the native-fee fixtures with the mainnet.test verifier that exported
# them. The sn release gate builds both (sn/scripts/build-mainnet-exports.sh)
# and runs the roots with them exported as URNETWORK_NATIVE_FEE_SETTLEMENT_*.
# With none of those set, the roots build the same exports from ../sn the
# gate's way themselves (internal/nativefeefixture), so on Linux they always
# run. That verifier builds and runs only on Linux (sn's mainnet tests and
# nativefee.Invoke), and an unexecuted fixture is never a skip, so on another
# host the roots fail by design. There, with none of the variables set, this
# leaves exactly those six out of the two package runs and says so; with any
# of them set, even empty, they run as the caller asked. go test keeps only the
# last -skip, so a caller's own -skip replaces this one.
native_fee_model_skip_args=()
native_fee_controller_skip_args=()
if [[ "$(uname -s)" != Linux &&
      -z "${URNETWORK_NATIVE_FEE_SETTLEMENT_FIXTURE_DIRECTORY+set}${URNETWORK_NATIVE_FEE_SETTLEMENT_VERIFIER+set}${URNETWORK_NATIVE_FEE_SETTLEMENT_VERIFIER_SHA256+set}" ]]; then
    native_fee_model_skip_args=(-skip '^TestStNativeFeeSettlementPublic(OwnedProofRetainsAllOriginals|MissingRefundKeepsFullCeiling|ProofRecoversLostSignerResult|FractionalConflictRevokesCredit)$')
    native_fee_controller_skip_args=(-skip '^TestNativeFeeControllerPublic(ProofSettlesWithoutSignerConfiguration|UnknownRefundKeepsOriginalCeiling)$')
    echo "test.sh: the six public native-fee roots are left out of ./model and ./controller: their sn verifier runs only on Linux, and this host is $(uname -s)" >&2
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
        package_args=("${native_fee_model_skip_args[@]}")
    elif [[ $d == ./controller ]]; then
        package_args=("${native_fee_controller_skip_args[@]}")
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
