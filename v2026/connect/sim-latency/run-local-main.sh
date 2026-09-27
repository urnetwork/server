#!/usr/bin/env bash

env=main
domain=bringyour.com

# You must port-forward the main PostgreSQL and Redis endpoints to 127.0.0.1.
# For example:
# ssh -i ${main-data-key} -L 5432:127.0.0.1:5432 \
#   -L 6379:127.0.0.1:6379 by@${main-data-host} -N

export WARP_HOST="127.0.0.1"
export WARP_BLOCK="sim"
export WARP_SERVICE="sim"
export WARP_VERSION="0.0.0-local"
export WARP_ENV="$env"
export WARP_DOMAIN="$domain"
export BRINGYOUR_POSTGRES_HOSTNAME="127.0.0.1"
export BRINGYOUR_REDIS_HOSTNAME="127.0.0.1"

exec go run . "$@"

