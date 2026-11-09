#!/usr/bin/env bash

# Runs the dns API block against the local stack, or, with --role edge, a dns edge
# that follows it. Start ../../local/run-local.sh first and wait for 'Local
# environment is up': local-env.sh attests the launcher's endpoints, and without
# that attestation this WARP_ENV=local process refuses every PostgreSQL and Redis
# dial (test_env_dial_guard.go). The edge dials neither.
#
# This binary releases no DNSKEY window until DNS-M5-19, so an edge it feeds stays
# not ready. ../../local/run-local-dns.sh runs both roles on the launcher's alias
# with a local key set, and that edge answers.

export WARP_HOST="127.0.0.1"
export WARP_BLOCK="local"
export WARP_SERVICE="dns"
export WARP_VERSION="0.0.0-local"
export WARP_ENV="local"
export BRINGYOUR_POSTGRES_HOSTNAME="local-pg.bringyour.com"
export BRINGYOUR_REDIS_HOSTNAME="local-redis.bringyour.com"
source "$(dirname -- "${BASH_SOURCE[0]}")/../../local/local-env.sh" || exit 1
role="api"
for arg in "$@"; do
    case "$arg" in
        --role=edge) role="edge" ;;
    esac
done
if [ "$role" = "edge" ]; then
    export WARP_SERVICE="dnsedge-1"
fi
go run . --role="$role" "$@"
