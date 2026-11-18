#!/usr/bin/env bash
# Builds the containers server/cli/dns's conformance tests run (DNS-M5-24, §22.9's DNS row):
# sidecar-test-dns-unbound, sidecar-test-dns-knot-resolver and sidecar-test-dns-bind, each tagged :local.
# Each build fetches its packages from Alpine's mirrors, so a person runs this, never an agent of the build
# (BRIEF.md: nothing but Go modules comes from outside). It prints each tool's version, which a conformance
# result names. zonemaster is not built here: it asks port 53 alone, which a test edge on a developer host
# does not bind, so it runs at T6 over test roots (tests/test-main.sh).
#
# usage: build.sh [tool...]    tools: unbound knot-resolver bind; none builds all three
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
tools=("$@")
if [ ${#tools[@]} -eq 0 ]; then
  tools=(unbound knot-resolver bind)
fi
for tool in "${tools[@]}"; do
  case $tool in
    unbound|knot-resolver|bind) ;;
    *) echo "build.sh: no such tool: $tool" >&2; exit 64 ;;
  esac
  docker build --pull=false -t "sidecar-test-dns-$tool:local" -f "$here/$tool.Dockerfile" "$here"
done
