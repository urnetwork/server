#!/usr/bin/env bash

# Run the already-built release control binary against Main through the
# builder's us-fmt LAN settings. Database and Redis overrides are cleared so
# the selected host stanza remains the sole routing authority.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

: "${WARP_VERSION:?WARP_VERSION must identify the release being deployed}"

export WARP_ENV="main"
export WARP_SERVICE="bringyourctl"
export WARP_DOMAIN="${WARP_MAIN_DOMAIN:-bringyour.com}"
export WARP_HOST="${WARP_MAIN_LAN_HOST:-builder}"
unset BRINGYOUR_POSTGRES_HOSTNAME
unset BRINGYOUR_REDIS_HOSTNAME

if [[ -n "${BRINGYOURCTL_BINARY:-}" ]]; then
    binary="$BRINGYOURCTL_BINARY"
else
    host_goos="$(go env GOOS)"
    host_goarch="$(go env GOARCH)"
    binary="$script_dir/build/$host_goos/$host_goarch/bringyourctl"
fi

if [[ ! -x "$binary" ]]; then
    echo "Versioned bringyourctl binary is not executable: $binary" >&2
    exit 1
fi

exec "$binary" "$@"
