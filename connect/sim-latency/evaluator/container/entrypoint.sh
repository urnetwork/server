#!/usr/bin/env bash

set -Eeuo pipefail

readonly IDENTITY=/opt/urnetwork/image-identity.json
readonly OFFICIAL_RUN=/usr/local/libexec/competition-official-run

# Every WARP_ENV=local process dials PostgreSQL and Redis only at the endpoints
# named by WARP_TEST_ENV_ATTESTED_ADDRS and refuses every dial without them
# (server test_env_dial_guard.go). In this container the launcher is the
# Compose job: the migrator and the runner reach the job's own postgres and
# redis services on its internal network. Each vault authority is resolved
# once, must name exactly one address that is not loopback, and is exported for
# the rest of the job; the Go processes then dial only those literals.
attest_job_services() {
    local vault_dir="${WARP_HOME:?}/vault/local"
    local endpoints=()
    local resource
    local authority
    local host
    local port
    local addresses
    local address

    for resource in pg.yml redis.yml; do
        authority="$(sed -n -E 's/^authority:[[:space:]]*"?([^"[:space:]]*)"?[[:space:]]*$/\1/p' "$vault_dir/$resource")"
        if [[ ! "$authority" =~ ^([A-Za-z0-9][A-Za-z0-9.-]*):([1-9][0-9]{0,4})$ ]] || (( BASH_REMATCH[2] > 65535 )); then
            printf 'competition-container: %s needs one host:port authority\n' "$resource" >&2
            return 1
        fi
        host="${BASH_REMATCH[1]}"
        port="${BASH_REMATCH[2]}"
        addresses="$(getent ahosts "$host" | awk '{ print $1 }' | LC_ALL=C sort -u)" || {
            printf 'competition-container: %s did not resolve\n' "$host" >&2
            return 1
        }
        if [[ -z "$addresses" || "$addresses" == *$'\n'* ]]; then
            printf 'competition-container: %s must resolve to exactly one address to be attested\n' "$host" >&2
            return 1
        fi
        address="$addresses"
        if [[ "$address" =~ ^127\. || "$address" =~ ^::[Ff]{4}:127\. || "$address" =~ ^[0:]*:0*1$ ]]; then
            printf 'competition-container: %s resolves to loopback %s; refusing to attest it\n' "$host" "$address" >&2
            return 1
        fi
        if [[ "$address" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]]; then
            endpoints+=("$address:$port")
        elif [[ "$address" == *:* && "$address" =~ ^[0-9A-Fa-f:]+$ ]]; then
            endpoints+=("[$address]:$port")
        else
            printf 'competition-container: %s resolves to an unexpected address %s\n' "$host" "$address" >&2
            return 1
        fi
    done
    WARP_TEST_ENV_ATTESTED_ADDRS="$(IFS=,; printf '%s' "${endpoints[*]}")"
    export WARP_TEST_ENV_ATTESTED_ADDRS
}

# --- dispatch ---

case "${1:-}" in
    identity)
        exec /usr/bin/jq --sort-keys . "$IDENTITY"
        ;;
    migrate)
        export WARP_HOME=/runtime
        export WARP_ENV=local
        export WARP_SERVICE=sim
        export WARP_BLOCK=sim
        export WARP_HOST=127.0.0.1
        attest_job_services
        exec /usr/local/libexec/competitiondbinit
        ;;
    run)
        attest_job_services
        export APEX_SERVER_ROOT=/workspace/server
        exec "$OFFICIAL_RUN" "$1"
        ;;
    preflight|baseline|score|finalize)
        export APEX_SERVER_ROOT=/workspace/server
        exec "$OFFICIAL_RUN" "$1"
        ;;
    *)
        printf '%s\n' 'usage: competition-container {identity|migrate|preflight|run|baseline|score|finalize}' >&2
        exit 2
        ;;
esac
