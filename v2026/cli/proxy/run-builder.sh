#!/usr/bin/env bash

env=main

export GOEXPERIMENT=greenteagc;
export WARP_DOMAIN="bringyour.com";
export WARP_SERVICE="proxy";
export WARP_VERSION="0.0.0-local";
export WARP_ENV="$env";
# FIXME this is temporary until the final deploy is worked out
export WARP_HOST="cosmic.bringyour.com";
export WARP_BLOCK="g1";
export BRINGYOUR_POSTGRES_HOSTNAME="192.168.51.43";
export BRINGYOUR_REDIS_HOSTNAME="192.168.51.43";
while [ 1 ]; do
	go run . "$@"
done
