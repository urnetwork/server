#!/usr/bin/env bash

export WARP_DOMAIN="bringyour.com";
export WARP_HOST="127.0.0.1";
export WARP_BLOCK="local";
export WARP_SERVICE="proxy";
export WARP_VERSION="0.0.0-local";
export WARP_ENV="local";
# FIXME this is temporary until the final deploy is worked out
export WARP_HOST="cosmic.bringyour.com"
export BRINGYOUR_POSTGRES_HOSTNAME="local-pg.bringyour.com";
export BRINGYOUR_REDIS_HOSTNAME="local-redis.bringyour.com";
go run . "$@"
