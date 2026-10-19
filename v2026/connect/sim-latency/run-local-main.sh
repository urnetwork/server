#!/usr/bin/env bash

env=main
domain=bringyour.com

# You must port-forward the main PostgreSQL and Redis endpoints to 127.0.0.1,
# for example with xops/bin/by-patch-main-data. It forwards the 6379 entry and
# every Redis node port (6380-6427, on edge-6 and edge-5); the client dials
# each node on its own port at 127.0.0.1.

export WARP_HOST="127.0.0.1"
export WARP_BLOCK="sim"
export WARP_SERVICE="sim"
export WARP_VERSION="0.0.0-local"
export WARP_ENV="$env"
export WARP_DOMAIN="$domain"
export BRINGYOUR_POSTGRES_HOSTNAME="127.0.0.1"
export BRINGYOUR_REDIS_HOSTNAME="127.0.0.1"

exec go run . "$@"

