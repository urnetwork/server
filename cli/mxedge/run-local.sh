#!/usr/bin/env bash

# Runs mxedge's platform listener (--role platform) against the local stack. Start
# ../../local/run-local.sh first and wait for 'Local environment is up': local-env.sh
# attests the launcher's endpoints, and without that attestation this WARP_ENV=local
# process refuses every PostgreSQL and Redis dial (test_env_dial_guard.go).
#
# The block reads config/local/mxedge.yml, and platform-mail.pem and platform-mail.key
# from --secret_dir: unless one is given, this makes a throwaway self-signed pair for
# the local report host in a temporary directory. TCP 25 is served on 2525 here and
# /status on 8080, and the port expects the lb's PROXY header, so a local sender
# writes "PROXY TCP4 <sender> <address> <port> 25" first. Without config/local/smtpedge.yml
# the block has no report intake (report_intake.go) and every recipient it answers for
# gets 451 4.3.0, and until config/local/sidecar.yml opens the mxedge gate so does every
# recipient, and a plaintext transaction gets 451 4.7.0.

export WARP_HOST="127.0.0.1"
export WARP_BLOCK="local"
export WARP_SERVICE="mxedge-platform"
export WARP_VERSION="0.0.0-local"
export WARP_ENV="local"
export BRINGYOUR_POSTGRES_HOSTNAME="local-pg.bringyour.com"
export BRINGYOUR_REDIS_HOSTNAME="local-redis.bringyour.com"
source "$(dirname -- "${BASH_SOURCE[0]}")/../../local/local-env.sh" || exit 1

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
