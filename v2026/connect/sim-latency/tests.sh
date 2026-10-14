#!/usr/bin/env bash

set -o pipefail

script_path="${BASH_SOURCE[0]}"
script_dir="${script_path%/*}"
[[ "$script_dir" != "$script_path" ]] || script_dir="."
script_dir="$(cd -- "$script_dir" >/dev/null 2>&1 && pwd)" || exit $?
server_dir="${script_dir%/*}"
server_dir="${server_dir%/*}"
cd "$script_dir"
source "$server_dir/test-env.sh" || exit $?

mkdir -p profile
match="/${script_dir##*/}/\\S*\\.go|^\\S*_test.go"

# The six-epoch lifecycle is wall-clock shaped but uses an explicit test clock.
# Run it first without race scheduling so every package invocation proves the
# complete admission, drain, review, promotion, and final-exit contract.
go test -timeout 5m -v -run '^TestRunMainCompleteSixEpochLifecycle$' \
    | grep --color=always -e "^" -e "$match"
test_status=${PIPESTATUS[0]}
if [[ $test_status != 0 ]]; then
    exit $test_status
fi

GORACE="log_path=profile/race.out halt_on_error=1" \
    go test -timeout 900m -v -race \
    -cpuprofile profile/cpu -memprofile profile/memory "$@" \
    | grep --color=always -e "^" -e "$match"
test_status=${PIPESTATUS[0]}
if [[ $test_status != 0 ]]; then
    exit $test_status
fi
