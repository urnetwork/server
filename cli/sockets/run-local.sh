#!/usr/bin/env bash

# Runs the sockets block against the local stack. Start ../../local/run-local.sh first
# and wait for 'Local environment is up': local-env.sh attests the launcher's
# endpoints, and without that attestation this WARP_ENV=local process refuses
# every Redis dial (test_env_dial_guard.go). sockets holds no Postgres.
#
# The block reads socket-ticket-keys.yml and internal-gate-sockets.yml from the
# local vault, and reads api's internal gate at -api_url. Until api serves its
# internal projection route (ACL-M4-02) and Client-class tokens verify for sockets
# (plan-SOCK-client-auth) it answers every authenticated route unavailable, and
# its log says what it waits for. With no key file it stays not ready.

export WARP_HOST="127.0.0.1"
export WARP_BLOCK="local"
export WARP_SERVICE="sockets"
export WARP_VERSION="0.0.0-local"
export WARP_ENV="local"
# the preflight checks both of the launcher's stores, though sockets dials only Redis
export BRINGYOUR_POSTGRES_HOSTNAME="local-pg.bringyour.com"
export BRINGYOUR_REDIS_HOSTNAME="local-redis.bringyour.com"
source "$(dirname -- "${BASH_SOURCE[0]}")/../../local/local-env.sh" || exit 1
go run . "$@"
