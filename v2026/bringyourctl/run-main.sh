#!/usr/bin/env bash

env=main
domain=bringyour.com

# you must port forward to the main db and redis from 127.0.0.1, e.g. with
# xops/bin/by-patch-main-data. It forwards the 6379 entry and every Redis node
# port (6380-6427, on edge-6 and edge-5); the client dials each node on its own
# port at 127.0.0.1

export WARP_SERVICE="bringyourctl"; \
export WARP_VERSION="0.0.0-local"; \
export WARP_ENV="$env"; \
export WARP_DOMAIN="$domain"; \
export BRINGYOUR_POSTGRES_HOSTNAME="127.0.0.1"; \
export BRINGYOUR_REDIS_HOSTNAME="127.0.0.1"; \
go run . "$@"
