#!/usr/bin/env zsh

# List local Go test directories for test.sh. Acceptance-owned source,
# commands, and artifacts belong to the separate acceptance harness and are
# pruned before test-file discovery.
server_dir=${0:A:h}
cd "$server_dir" || exit $?

find . \
    -type d -iname '*acceptance*' -prune -o \
    -path './connect/sim-latency/eval-*' -prune -o \
    -path './connect/sim-latency/baseline' -prune -o \
    -iname '*_test.go' -print | \
    xargs -n 1 dirname | sort | uniq
