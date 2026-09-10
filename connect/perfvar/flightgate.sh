#!/usr/bin/env bash
# Shell entry for the pinned-provider collapse campaign driver and readout.
#   ./flightgate.sh campaign -out DIR -arm stock=/path/to/connect [-arm a1=...] [filters]
#   ./flightgate.sh readout  -out DIR [-md FILE]
# See flightgate/main.go and PERFVAR.md ("Mixed P2P and exchange routes").
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
server="$(cd "$here/../.." && pwd)"
cd "$server"
exec go run ./connect/perfvar/flightgate -server "$server" "$@"
