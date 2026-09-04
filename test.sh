#!/usr/bin/env bash

set -o pipefail

script_path="${BASH_SOURCE[0]}"
script_dir="${script_path%/*}"
[[ "$script_dir" != "$script_path" ]] || script_dir="."
script_dir="$(cd -- "$script_dir" >/dev/null 2>&1 && pwd)" || exit $?
cd "$script_dir" || exit $?
source "$script_dir/test-env.sh" || exit $?

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
    go test -timeout 30m -v "$@" | grep --line-buffered --color=always -e "^" -e "$match"
    test_status=${PIPESTATUS[0]}
    if [[ $test_status != 0 ]]; then
        exit "$test_status"
    fi
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
    go test -timeout 0 -p=1 -parallel=1 -v "$@" | grep --line-buffered --color=always -e "^" -e "$match"
    test_status=${PIPESTATUS[0]}
    if [[ $test_status != 0 ]]; then
        exit "$test_status"
    fi
    GORACE="log_path=profile/race.out halt_on_error=1" go test -timeout 30m -p=1 -parallel=1 -short -v -race -cpuprofile profile/cpu -memprofile profile/memory "$@" | grep --line-buffered --color=always -e "^" -e "$match"
    test_status=${PIPESTATUS[0]}
    if [[ $test_status != 0 ]]; then
        exit "$test_status"
    fi
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

test_directories="$(./test-dirs.sh)" || exit $?
while IFS= read -r d; do
    [[ -n "$d" ]] || continue
    if [[ $d == $proxy_dir || $d == $perfvar_dir ]]; then
        # run separately above with each integration package's timing contract
        continue
    fi
    # if [[ $1 == "" || $1 == `basename $d` ]]; then
        pushd "$d"
        # highlight source files in this dir
        match="/${PWD##*/}/\\S*\.go\|^\\S*_test.go"
        # go test -v "$@" | grep --color=always -e "^" -e "$match"
        GORACE="log_path=profile/race.out halt_on_error=1" go test -timeout 0 -v -race -cpuprofile profile/cpu -memprofile profile/memory "$@" | grep --line-buffered --color=always -e "^" -e "$match"
        test_status=${PIPESTATUS[0]}
        if [[ $test_status != 0 ]]; then
            exit "$test_status"
        fi
        popd
    # fi
done <<< "$test_directories"
# stdbuf -i0 -o0 -e0

# ./test.sh -run 'pattern'
# ./test.sh -short
