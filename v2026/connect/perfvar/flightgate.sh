#!/usr/bin/env bash
# Shell entry for the pinned-provider collapse campaign driver and readout.
#   ./flightgate.sh campaign -out DIR -arm stock=/path/to/connect [-arm a1=...] [filters]
#   ./flightgate.sh readout  -out DIR [-md FILE]
# See flightgate/main.go and PERFVAR.md ("Mixed P2P and exchange routes").
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
server="$(cd "$here/../.." && pwd)"
cd "$server"
subcommand="${1:-}"
if [ "$subcommand" = "campaign" ]; then
  shift
  exec go run ./connect/perfvar/flightgate campaign -server "$server" "$@"
fi
exec go run ./connect/perfvar/flightgate "$@"
