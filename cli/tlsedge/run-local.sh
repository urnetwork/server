#!/usr/bin/env bash

# Runs the tlsedge block against the local stack. Start ../../local/run-local.sh first
# and wait for 'Local environment is up': local-env.sh attests the launcher's
# endpoints, and without that attestation this WARP_ENV=local process refuses
# every Postgres and Redis dial (test_env_dial_guard.go).
#
# The block reads tlsedge.yml and sidecar.yml from the local config, and
# internal-gate-tlsedge.yml and internal-gate-edge.yml from the local vault. It
# reads api's internal gate at -api_url, for the quota surface and for its
# platform clients' jwts (config/local/edge.yml lists this host), and sends
# every Find to the sockets block at -sockets_url. Until api answers its clients
# and all of them are connected it stays not ready, and its log says what it
# waits for. It listens on 443, 8443, 8883 and 5671 behind a PROXY header, so a
# visitor comes through the local lb, never straight to these ports.

export WARP_HOST="127.0.0.1"
export WARP_BLOCK="local"
export WARP_SERVICE="tlsedge"
export WARP_VERSION="0.0.0-local"
export WARP_ENV="local"
export BRINGYOUR_POSTGRES_HOSTNAME="local-pg.bringyour.com"
export BRINGYOUR_REDIS_HOSTNAME="local-redis.bringyour.com"
source "$(dirname -- "${BASH_SOURCE[0]}")/../../local/local-env.sh" || exit 1
go run . "$@"
