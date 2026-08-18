#!/usr/bin/env bash

set -Eeuo pipefail

readonly IDENTITY=/opt/urnetwork/image-identity.json
readonly OFFICIAL_RUN=/usr/local/libexec/competition-official-run

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
        exec /usr/local/libexec/competitiondbinit
        ;;
    preflight|run|baseline|score|finalize)
        export APEX_SERVER_ROOT=/workspace/server
        exec "$OFFICIAL_RUN" "$1"
        ;;
    *)
        printf '%s\n' 'usage: competition-container {identity|migrate|preflight|run|baseline|score|finalize}' >&2
        exit 2
        ;;
esac
