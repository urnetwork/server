#!/usr/bin/env bash

env=main

# you must port forward to the main db and redis from 127.0.0.1
# e.g. ssh -i ${main-data-key} -L 5432:127.0.0.1:5432 -L 6379:127.0.0.1:6379 by@${main-data-host} -N

export WARP_SERVICE="taskworker"; \
export WARP_VERSION="0.0.0-local"; \
export WARP_ENV="$env"; \
export BRINGYOUR_POSTGRES_HOSTNAME="127.0.0.1"; \
export BRINGYOUR_REDIS_HOSTNAME="127.0.0.1"; \
go run . "$@"
