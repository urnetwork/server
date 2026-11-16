#!/usr/bin/env bash

set -o pipefail

script_path="${BASH_SOURCE[0]}"
script_dir="${script_path%/*}"
[[ "$script_dir" != "$script_path" ]] || script_dir="."
script_dir="$(cd -- "$script_dir" >/dev/null 2>&1 && pwd)" || exit $?
server_dir="${script_dir%/*}"
cd "$script_dir" || exit $?
source "$server_dir/test-env.sh" || exit $?

test_directories="$(find . -iname '*_test.go' -print | while IFS= read -r test_file; do dirname "$test_file"; done | sort -u)" || exit $?
while IFS= read -r d; do
    [[ -n "$d" ]] || continue
    # if [[ $1 == "" || $1 == `basename $d` ]]; then
        pushd "$d"
        # highlight source files in this dir
        match="/${PWD##*/}/\\S*\.go\|^\\S*_test.go"
        # go test -v "$@" | grep --color=always -e "^" -e "$match"
        GORACE="log_path=profile/race.out halt_on_error=1" go test -v -race -cpuprofile profile/cpu -memprofile profile/memory -timeout 180m "$@" | grep --color=always -e "^" -e "$match"
        test_status=${PIPESTATUS[0]}
        if [[ $test_status != 0 ]]; then
            exit "$test_status"
        fi
        popd
    # fi
done <<< "$test_directories"
# stdbuf -i0 -o0 -e0 
