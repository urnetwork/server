#!/usr/bin/env bash

# Runs one of mxedge's roles against the local stack: "router" as the first argument runs the router
# (--role router), anything else the platform listener (--role platform). Start ../../local/run-local.sh
# first and wait for 'Local environment is up': local-env.sh attests the launcher's endpoints, and without
# that attestation this WARP_ENV=local process refuses every PostgreSQL and Redis dial
# (test_env_dial_guard.go).
#
# Either role reads config/local/mxedge.yml. TCP 25 is served on 2525 here and /status on 8080, and the
# port expects the lb's PROXY header, so a local sender writes "PROXY TCP4 <sender> <address> <port> 25"
# first. Until config/local/sidecar.yml opens the mxedge gate every session that reaches TLS closes in the
# handshake, and a plaintext transaction gets 451 4.7.0.
#
# The platform role reads platform-mail.pem and platform-mail.key from --secret_dir: unless one is given,
# this makes a throwaway self-signed pair for the local report host in a temporary directory. Without
# config/local/smtpedge.yml the block has no report intake (report_intake.go) and every recipient it
# answers for gets 451 4.3.0. It files role mail's notices at api's internal gate at -api_url on
# internal-gate-mxedge-platform.yml from the local vault (QD-1070); without either it files none and
# answers role mail as before.
#
# The router reads mxedge.yml's route_root, internal-gate-mxedge.yml and internal-gate-edge.yml from the
# local vault, api's internal gate at -api_url for the quota surface and its platform clients' jwts
# (config/local/edge.yml lists this host), and sends every Find to the sockets block at -sockets_url. Until
# api answers its clients and all of them are connected it stays not ready, and its log says what it waits
# for. It holds no TLS key and reads no file of the platform role's.

role="platform"
if [ "$1" = "router" ]; then
    role="router"
    shift
fi

export WARP_HOST="127.0.0.1"
export WARP_BLOCK="local"
export WARP_SERVICE="mxedge-platform"
if [ "$role" = "router" ]; then
    export WARP_SERVICE="mxedge"
fi
export WARP_VERSION="0.0.0-local"
export WARP_ENV="local"
export BRINGYOUR_POSTGRES_HOSTNAME="local-pg.bringyour.com"
export BRINGYOUR_REDIS_HOSTNAME="local-redis.bringyour.com"
source "$(dirname -- "${BASH_SOURCE[0]}")/../../local/local-env.sh" || exit 1

if [ "$role" = "router" ]; then
    go run . --role=router --port=8080 --smtp_port=2525 "$@"
    exit $?
fi

secret_dir=""
for arg in "$@"; do
    case "$arg" in
        --secret_dir=*) secret_dir="${arg#--secret_dir=}" ;;
    esac
done
if [ -z "$secret_dir" ]; then
    secret_dir="$(mktemp -d)" || exit 1
    trap 'rm -rf "$secret_dir"' EXIT
    openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 30 \
        -subj "/CN=rpt.report.example" -addext "subjectAltName=DNS:rpt.report.example" \
        -keyout "$secret_dir/platform-mail.key" -out "$secret_dir/platform-mail.pem" 2>/dev/null || exit 1
fi
go run . --role=platform --port=8080 --smtp_port=2525 --secret_dir="$secret_dir" "$@"
