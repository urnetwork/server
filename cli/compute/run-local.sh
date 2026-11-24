#!/usr/bin/env bash

# Runs the compute block against the local stack. Start ../../local/run-local.sh first
# and wait for 'Local environment is up': local-env.sh attests the launcher's
# endpoints, and without that attestation this WARP_ENV=local process refuses
# every Postgres and Redis dial (test_env_dial_guard.go).
#
# The block reads file-clusters-compute.yml, the first metadata cluster's read-only
# credentials for proj/ and rev/, from the local vault, and holds Postgres. It stays
# not ready, serving /status alone and saying why in its log, until this binary links
# MG-C1 and MG-C2 (the sidecar_pending tag, which local-env.sh's GOFLAGS sets) and the
# local database is at their head, and every route answers 503 while config/local/sidecar.yml's
# compute gate stays closed, as it is written. No area's handler is built yet, so an
# admitted request to any route answers 503 not_built.

export WARP_HOST="127.0.0.1"
export WARP_BLOCK="local"
export WARP_SERVICE="compute"
export WARP_VERSION="0.0.0-local"
export WARP_ENV="local"
export BRINGYOUR_POSTGRES_HOSTNAME="local-pg.bringyour.com"
export BRINGYOUR_REDIS_HOSTNAME="local-redis.bringyour.com"
source "$(dirname -- "${BASH_SOURCE[0]}")/../../local/local-env.sh" || exit 1
go run . "$@"
