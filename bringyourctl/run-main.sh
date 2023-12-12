#!/usr/bin/env bash

env=main

export WARP_VERSION="0.0.0-local"; \
export WARP_ENV="$env"; \
export BRINGYOUR_POSTGRES_HOSTNAME="127.0.0.1"; \
export BRINGYOUR_REDIS_HOSTNAME="127.0.0.1"; \
./bringyourctl "$@"
