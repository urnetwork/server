#!/usr/bin/env bash

env=main

export WARP_DOMAIN="bringyour.com"; \
export WARP_SERVICE="proxy"; \
export WARP_VERSION="0.0.0-local"; \
export WARP_ENV="$env"; \
export BRINGYOUR_POSTGRES_HOSTNAME="192.168.51.43"; \
export BRINGYOUR_REDIS_HOSTNAME="192.168.51.43"; \
go run . "$@"
