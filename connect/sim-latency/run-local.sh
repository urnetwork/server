#!/usr/bin/env bash
#
# run-local.sh -- run sim-latency against the local stack with the standard
# local-env variables set (mirrors server/cli/*/run-local.sh). Requires
# server/local/run-local.sh to be running (postgres + redis).
#
# Usage: ./run-local.sh <init|run|fleet> [args...]
#   ./run-local.sh init --count 2000 --clients 200 --out providers.yml
#   ./run-local.sh run --providers providers.yml > results.csv

export WARP_HOST="127.0.0.1"
export WARP_BLOCK="sim"
export WARP_SERVICE="sim"
export WARP_VERSION="0.0.0-sim"
export WARP_ENV="local"
export BRINGYOUR_POSTGRES_HOSTNAME="local-pg.bringyour.com"
export BRINGYOUR_REDIS_HOSTNAME="local-redis.bringyour.com"

exec go run . "$@"
