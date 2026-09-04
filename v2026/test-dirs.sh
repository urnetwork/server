#!/usr/bin/env bash

# List local Go test directories for test.sh. Acceptance-owned source,
# commands, and artifacts belong to the separate acceptance harness and are
# pruned before test-file discovery.
set -o pipefail

for command_name in find dirname sort; do
    if ! command -v "$command_name" >/dev/null 2>&1; then
        printf 'test-dirs.sh: missing prerequisite: %s\n' "$command_name" >&2
        exit 1
    fi
done

script_path="${BASH_SOURCE[0]}"
if [[ "$script_path" == */* ]]; then
    script_dir="${script_path%/*}"
else
    script_dir="."
fi
server_dir="$(cd -- "$script_dir" >/dev/null 2>&1 && pwd)" || exit $?
cd "$server_dir" || exit $?

find . \
    -type d -iname '*acceptance*' -prune -o \
    -path './connect/sim-latency/eval-*' -prune -o \
    -path './connect/sim-latency/baseline' -prune -o \
    -iname '*_test.go' -print | \
    while IFS= read -r test_file; do dirname "$test_file"; done | sort -u
