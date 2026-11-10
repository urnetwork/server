#!/usr/bin/env bash

# Runs the gkm block against the local stack. Start ../../local/run-local.sh first
# and wait for 'Local environment is up': local-env.sh attests the launcher's
# endpoints, and without that attestation this WARP_ENV=local process refuses
# every Redis dial (test_env_dial_guard.go). gkm holds no Postgres.
#
# The block reads file-clusters-gkm.yml from the local vault. Until gkm's
# authorization state lands (GkmAuthState over the gate cache, ACL-M2-14's
# HonorScope) it stays not ready, serving /status alone, and its log says what it
# waits for.

export WARP_HOST="127.0.0.1"
export WARP_BLOCK="local"
export WARP_SERVICE="gkm"
export WARP_VERSION="0.0.0-local"
export WARP_ENV="local"
# the preflight checks both of the launcher's stores, though gkm dials only Redis
export BRINGYOUR_POSTGRES_HOSTNAME="local-pg.bringyour.com"
export BRINGYOUR_REDIS_HOSTNAME="local-redis.bringyour.com"
source "$(dirname -- "${BASH_SOURCE[0]}")/../../local/local-env.sh" || exit 1
go run . "$@"
