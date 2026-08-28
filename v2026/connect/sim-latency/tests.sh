#!/usr/bin/env zsh

set -o pipefail

script_dir=${0:a:h}
cd "$script_dir"

export WARP_ENV="local"
export WARP_SERVICE="test"
export WARP_DOMAIN="bringyour.com"
export WARP_BLOCK="test"
export WARP_VERSION="0.0.0"
export BRINGYOUR_POSTGRES_HOSTNAME="local-pg.bringyour.com"
export BRINGYOUR_REDIS_HOSTNAME="local-redis.bringyour.com"

mkdir -p profile
match="/$(basename "$script_dir")/\\S*\\.go|^\\S*_test.go"
GORACE="log_path=profile/race.out halt_on_error=1" \
    go test -timeout 900m -v -race \
    -cpuprofile profile/cpu -memprofile profile/memory "$@" \
    | grep --color=always -e "^" -e "$match"
test_status=${pipestatus[1]}
if [[ $test_status != 0 ]]; then
    exit $test_status
fi
