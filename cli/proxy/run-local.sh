#!/usr/bin/env bash

# Runs the proxy service against the local stack. Start
# ../../local/run-local.sh first and wait for 'Local environment is up':
# local-env.sh attests the launcher's endpoints, and without that attestation
# this WARP_ENV=local process refuses every PostgreSQL and Redis dial
# (test_env_dial_guard.go).

export WARP_DOMAIN="bringyour.com"
export WARP_HOST="127.0.0.1"
export WARP_BLOCK="local"
export WARP_SERVICE="proxy"
export WARP_VERSION="0.0.0-local"
export WARP_ENV="local"
# FIXME this is temporary until the final deploy is worked out
export WARP_HOST="cosmic.bringyour.com"
export BRINGYOUR_POSTGRES_HOSTNAME="local-pg.bringyour.com"
export BRINGYOUR_REDIS_HOSTNAME="local-redis.bringyour.com"
source "$(dirname -- "${BASH_SOURCE[0]}")/../../local/local-env.sh" || exit 1
go run . "$@"
