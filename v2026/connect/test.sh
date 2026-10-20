#!/usr/bin/env bash

set -o pipefail

script_path="${BASH_SOURCE[0]}"
script_dir="${script_path%/*}"
[[ "$script_dir" != "$script_path" ]] || script_dir="."
script_dir="$(cd -- "$script_dir" >/dev/null 2>&1 && pwd)" || exit $?
server_dir="${script_dir%/*}"
cd "$script_dir" || exit $?
source "$server_dir/test-env.sh" || exit $?

# Share repository exclusions so immutable evidence is never treated as source.
test_directories="$("$server_dir/test-dirs.sh")" || exit $?
while IFS= read -r d; do
    case "$d" in
        ./connect|./connect/*) ;;
        *) continue ;;
    esac
    # if [[ $1 == "" || $1 == `basename $d` ]]; then
        pushd "$server_dir/${d#./}" || exit $?
        # highlight source files in this dir
        match="/${PWD##*/}/\\S*\.go\|^\\S*_test.go"
        # Self-contained helper packages do not register glog flags.
        GORACE="log_path=profile/race.out halt_on_error=1" go test -timeout 900m "$@" | grep --color=always -e "^" -e "$match"
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
