#!/usr/bin/env bash

export WARP_HOST="127.0.0.1"; \
export WARP_BLOCK="local"; \
export WARP_SERVICE="bringyourctl"; \
export WARP_VERSION="0.0.0-local"; \
export WARP_ENV="local"; \
export BRINGYOUR_POSTGRES_HOSTNAME="local-pg.bringyour.com"; \
export BRINGYOUR_REDIS_HOSTNAME="local-redis.bringyour.com"; \
go run . "$@"
