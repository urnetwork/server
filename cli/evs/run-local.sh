#!/usr/bin/env bash

# Runs the evs block against the local stack. Start ../../local/run-local.sh first
# and wait for 'Local environment is up': local-env.sh attests the launcher's
# endpoints, and without that attestation this WARP_ENV=local process refuses
# every Redis dial (test_env_dial_guard.go). evs holds no Postgres.
#
# The block reads file-clusters-evs.yml and evs-receipt-keys.json from the local
# vault, and asks chronyd for the time; pass --chrony_address=<host>:123 for a
# local one. Until QUOTA-M2-11's quota and ACL-M2-14's authorization state land it
# stays not ready, serving /status alone, and its log says what it waits for.

export WARP_HOST="127.0.0.1"
export WARP_BLOCK="local"
export WARP_SERVICE="evs"
export WARP_VERSION="0.0.0-local"
export WARP_ENV="local"
# the preflight checks both of the launcher's stores, though evs dials only Redis
export BRINGYOUR_POSTGRES_HOSTNAME="local-pg.bringyour.com"
export BRINGYOUR_REDIS_HOSTNAME="local-redis.bringyour.com"
source "$(dirname -- "${BASH_SOURCE[0]}")/../../local/local-env.sh" || exit 1
go run . "$@"
