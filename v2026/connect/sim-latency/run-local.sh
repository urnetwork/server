#!/usr/bin/env bash
#
# run-local.sh -- run sim-latency against the local stack with the standard
# local-env variables set (mirrors server/cli/*/run-local.sh). The commands that
# reach PostgreSQL and Redis (run, fleet, reset, and baseline when it measures
# fresh replicates) require server/local/run-local.sh to be running:
# local-env.sh attests the launcher's endpoints first, and without that
# attestation this WARP_ENV=local process refuses every PostgreSQL and Redis
# dial (test_env_dial_guard.go). The file-only commands need no stack.
#
# Usage: ./run-local.sh <command> [args...]
#   ./run-local.sh init --count 2000 --clients 200 --out providers.yml
#   ./run-local.sh run --epoch 0 --providers providers.yml > results.csv

export WARP_HOST="127.0.0.1"
export WARP_BLOCK="sim"
export WARP_SERVICE="sim"
export WARP_VERSION="0.0.0-sim"
export WARP_ENV="local"
export BRINGYOUR_POSTGRES_HOSTNAME="local-pg.bringyour.com"
export BRINGYOUR_REDIS_HOSTNAME="local-redis.bringyour.com"

# baseline with --runs only reads existing artifacts.
attest=0
case "${1:-}" in
    run|fleet|reset)
        attest=1
        ;;
    baseline)
        attest=1
        for arg in "$@"; do
            case "$arg" in
                --runs|--runs=*) attest=0 ;;
            esac
        done
        ;;
esac
if [[ "$attest" == 1 ]]; then
    source "$(dirname -- "${BASH_SOURCE[0]}")/../../local/local-env.sh" || exit 1
fi

exec go run . "$@"
