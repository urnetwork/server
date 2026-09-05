#!/usr/bin/env bash

set -o pipefail

script_path="${BASH_SOURCE[0]}"
script_dir="${script_path%/*}"
[[ "$script_dir" != "$script_path" ]] || script_dir="."
script_dir="$(cd -- "$script_dir" >/dev/null 2>&1 && pwd)" || exit $?
server_dir="${script_dir%/*}"
cd "$script_dir" || exit $?
workspace_root="${URNETWORK_ROOT:-${WARP_HOME:-${server_dir%/*}}}"
network_test_gate="$workspace_root/tests/network-intensive-suite-lock.sh"
if [[ ! -x "$network_test_gate" ]]; then
    echo "proxy test suite gate is missing or not executable: $network_test_gate" >&2
    exit 127
fi
if [[ "${URNETWORK_NETWORK_TEST_LOCK_HELD:-}" != 1 ]]; then
    exec "$network_test_gate" run-all-proxy -- "$script_dir/test.sh" "$@"
fi
if ! "$network_test_gate" --verify-held; then
    echo "proxy test suite inherited an invalid network-intensive lock" >&2
    exit 70
fi
source "$server_dir/test-env.sh" || exit $?

test_directories="$(find . -iname '*_test.go' -print | while IFS= read -r test_file; do dirname "$test_file"; done | sort -u)" || exit $?
while IFS= read -r d; do
    [[ -n "$d" ]] || continue
    # if [[ $1 == "" || $1 == `basename $d` ]]; then
        pushd "$d"
        # highlight source files in this dir
        match="/${PWD##*/}/\\S*\.go\|^\\S*_test.go"
        GORACE="log_path=profile/race.out halt_on_error=1" go test -timeout 900m "$@" -args -v 0 -logtostderr true | grep --color=always -e "^" -e "$match"
            # -trace profile/trace -coverprofile profile/cover 
        test_status=${PIPESTATUS[0]}
        if [[ $test_status != 0 ]]; then
            exit "$test_status"
        fi
        popd
    # fi
done <<< "$test_directories"
# stdbuf -i0 -o0 -e0 

# go tool trace profile/trace
# PPROF_BINARY_PATH=. go tool pprof profile/cpu

# store default.pgo
# https://go.dev/doc/pgo


# go test -short
