# Shared settings and helpers of the crypto regtest harness. Sourced by
# fetch.sh, up.sh and down.sh; not run directly. Bash 3.2 compatible (macOS).
#
# Everything this harness creates is named crypto-regtest-*: the compose
# project, its network, its containers and its volumes. Ports are published on
# 127.0.0.1 only.

set -o errexit -o nounset -o pipefail

HARNESS_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
CACHE_DIR="$HARNESS_DIR/.cache"
STATE_DIR="$HARNESS_DIR/.state"
SECRETS_DIR="$STATE_DIR/secrets"
HARNESS_JSON="$STATE_DIR/harness.json"

COMPOSE_PROJECT=crypto-regtest
COMPOSE_FILE="$HARNESS_DIR/docker-compose.yml"

# The release binaries are x86_64 Linux builds, run under Docker Desktop's
# amd64 emulation: the same bits as production. The base image is pinned by
# its dated tag.
BASE_IMAGE="debian:trixie-20261005-slim"

# Release pins, copied from xops/main/ansible/host_files/snow/crypto-pay/vars.yml
# (crypto_pay_releases), where each sha256 was checked against the publisher's
# signed checksums (Monero: binaryFate's signed hashes.txt; Zebra: SHA256SUMS
# with a sigstore bundle; zecd publishes no signature, only SHA256SUMS). A file
# whose sha256 differs is refused and deleted.
MONERO_RELEASE=monero-v0.18.5.4
MONERO_URL=https://downloads.getmonero.org/cli/monero-linux-x64-v0.18.5.4.tar.bz2
MONERO_SHA256=ba2928962c101b609434bf8c1b43ad59ebe96c5525977284ca599d8bf0f02d10
MONERO_STRIP=1
MONERO_VERSION_OUTPUT="Monero 'Fluorine Fermi' (v0.18.5.4-release)"

ZEBRAD_RELEASE=zebrad-6.4.2
ZEBRAD_URL=https://github.com/ZcashFoundation/zebra/releases/download/v6.4.2/zebrad-6.4.2-x86_64-unknown-linux-gnu.tar.gz
ZEBRAD_SHA256=505cab2c616dac1a5bc1c414716206a775f38f41ca6f70a60729df40c29e7b8b
ZEBRAD_STRIP=0
ZEBRAD_VERSION_OUTPUT="zebrad 6.4.2"

ZECD_RELEASE=zecd-0.8.2
ZECD_URL=https://github.com/zecrocks/zecd/releases/download/v0.8.2/zecd-0.8.2-linux-amd64.tar.gz
ZECD_SHA256=e9486f4b03a9fba10c976fac96e8bf6789fb10de48262a6da2b22c087c24eea0
ZECD_STRIP=1
ZECD_VERSION_OUTPUT="zecd 0.8.2"

# Host ports, on 127.0.0.1 only. Inside the compose network the daemons keep
# their own ports (in brackets).
PORT_MONEROD="${CRYPTO_REGTEST_PORT_MONEROD:-28881}"             # monerod [18081]
PORT_MONERO_FUNDER="${CRYPTO_REGTEST_PORT_MONERO_FUNDER:-28882}" # spending wallet-rpc [18082]
PORT_MONERO_VIEW="${CRYPTO_REGTEST_PORT_MONERO_VIEW:-28883}"     # view-only wallet-rpc [18088]
PORT_ZEBRAD="${CRYPTO_REGTEST_PORT_ZEBRAD:-28891}"               # zebrad [18232]
PORT_ZECD_FUNDER="${CRYPTO_REGTEST_PORT_ZECD_FUNDER:-28892}"     # spending zecd [18252]
PORT_ZECD_VIEW="${CRYPTO_REGTEST_PORT_ZECD_VIEW:-28893}"         # watch-only zecd [18262]
# up.sh --db only; never a port a production data tunnel forwards (5432, 6432,
# 6379-6427), which test-env.sh refuses on loopback
PORT_POSTGRES="${CRYPTO_REGTEST_PORT_POSTGRES:-28832}"           # postgres [5432]
PORT_REDIS="${CRYPTO_REGTEST_PORT_REDIS:-28879}"                 # redis [6379]

# zecd's regtest NU6.3 (Ironwood) height; Zebra's config must name the same.
ZECD_REGTEST_NU63_HEIGHT=8

# Containers, for the scripts and the Go test (docker restart of the view
# wallet).
CONTAINER_MONERO_VIEW=crypto-regtest-monero-view
CONTAINER_ZECD_VIEW=crypto-regtest-zecd-view

log() {
    printf '%s crypto-regtest: %s\n' "$(date -u +%H:%M:%S)" "$*" >&2
}

die() {
    log "ERROR: $*"
    exit 1
}

sha256_of() {
    shasum -a 256 "$1" | awk '{print $1}'
}

# A random alphanumeric secret. Never echoed by the scripts.
random_secret() {
    local secret
    secret="$(LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 40 || true)"
    [[ ${#secret} -eq 40 ]] || die "could not generate a secret"
    printf '%s' "$secret"
}

# Writes stdin to a 0600 file.
write_private() {
    local path="$1"
    (umask 077 && cat >"$path")
    chmod 600 "$path"
}

read_secret() {
    tr -d '\r\n' <"$1"
}

compose() {
    CRYPTO_REGTEST_BASE_IMAGE="$BASE_IMAGE" \
    CRYPTO_REGTEST_CACHE="$CACHE_DIR" \
    CRYPTO_REGTEST_STATE="$STATE_DIR" \
    CRYPTO_REGTEST_MONERO_BIN="$CACHE_DIR/bin/$MONERO_RELEASE" \
    CRYPTO_REGTEST_ZEBRAD_BIN="$CACHE_DIR/bin/$ZEBRAD_RELEASE" \
    CRYPTO_REGTEST_ZECD_BIN="$CACHE_DIR/bin/$ZECD_RELEASE" \
    CRYPTO_REGTEST_PORT_MONEROD="$PORT_MONEROD" \
    CRYPTO_REGTEST_PORT_MONERO_FUNDER="$PORT_MONERO_FUNDER" \
    CRYPTO_REGTEST_PORT_MONERO_VIEW="$PORT_MONERO_VIEW" \
    CRYPTO_REGTEST_PORT_ZEBRAD="$PORT_ZEBRAD" \
    CRYPTO_REGTEST_PORT_ZECD_FUNDER="$PORT_ZECD_FUNDER" \
    CRYPTO_REGTEST_PORT_ZECD_VIEW="$PORT_ZECD_VIEW" \
    CRYPTO_REGTEST_PORT_POSTGRES="$PORT_POSTGRES" \
    CRYPTO_REGTEST_PORT_REDIS="$PORT_REDIS" \
    CRYPTO_REGTEST_NU63_HEIGHT="$ZECD_REGTEST_NU63_HEIGHT" \
        docker compose --project-name "$COMPOSE_PROJECT" --file "$COMPOSE_FILE" "$@"
}

# ----- JSON-RPC from the host. Credentials go to curl on stdin (-K -), never
# on its command line; bodies that may hold a secret go through a 0600 file. -----

rpc_tmp_dir() {
    mkdir -p "$STATE_DIR/tmp"
    chmod 700 "$STATE_DIR/tmp"
    printf '%s' "$STATE_DIR/tmp"
}

# rpc_post <url> <auth: digest|basic|none> <login file or ""> <json body>
# Prints the response body. Fails on a transport error or a non-JSON answer.
rpc_post() {
    local url="$1" auth="$2" login_file="$3" body="$4"
    local body_file response status
    body_file="$(mktemp "$(rpc_tmp_dir)/body.XXXXXX")"
    printf '%s' "$body" >"$body_file"
    local curl_config=""
    if [[ "$auth" != none ]]; then
        curl_config="user = \"$(read_secret "$login_file")\""
    fi
    local auth_flag=()
    case "$auth" in
        digest) auth_flag=(--digest) ;;
        basic) auth_flag=(--basic) ;;
    esac
    response="$(printf '%s\n' "$curl_config" | curl --silent --show-error --max-time 900 \
        --config - ${auth_flag[@]+"${auth_flag[@]}"} \
        --header 'Content-Type: application/json' \
        --data-binary @"$body_file" \
        --write-out '\n%{http_code}' "$url")" || { rm -f "$body_file"; return 1; }
    rm -f "$body_file"
    status="${response##*$'\n'}"
    response="${response%$'\n'*}"
    if ! printf '%s' "$response" | jq -e . >/dev/null 2>&1; then
        log "non-JSON answer from $url (http $status)"
        return 1
    fi
    printf '%s' "$response"
}

# The result of a JSON-RPC answer, or a failure naming the method and the
# daemon's error (never the request, which may hold a secret).
rpc_result() {
    local method="$1" response="$2"
    if printf '%s' "$response" | jq -e '.error != null and .error != {}' >/dev/null 2>&1; then
        log "$method failed: $(printf '%s' "$response" | jq -c '.error')"
        return 1
    fi
    printf '%s' "$response" | jq -c '.result'
}

# monero_rpc <url> <login file or ""> <method> [params object]
monero_rpc() {
    local url="$1" login_file="$2" method="$3" params="${4:-}"
    [[ -n "$params" ]] || params='{}'
    local auth=digest
    [[ -n "$login_file" ]] || auth=none
    local response
    response="$(rpc_post "$url" "$auth" "$login_file" "{\"jsonrpc\":\"2.0\",\"id\":\"0\",\"method\":\"$method\",\"params\":$params}")" || return 1
    rpc_result "$method" "$response"
}

# zcash_rpc <url> <login file or ""> <method> [params array]
zcash_rpc() {
    local url="$1" login_file="$2" method="$3" params="${4:-}"
    [[ -n "$params" ]] || params='[]'
    local auth=basic
    [[ -n "$login_file" ]] || auth=none
    local response
    response="$(rpc_post "$url" "$auth" "$login_file" "{\"jsonrpc\":\"1.0\",\"id\":\"0\",\"method\":\"$method\",\"params\":$params}")" || return 1
    rpc_result "$method" "$response"
}

# wait_until <seconds> <description> <command...>: retries the command every 2 s.
wait_until() {
    local timeout="$1" what="$2"
    shift 2
    local deadline=$(($(date +%s) + timeout))
    until "$@" >/dev/null 2>&1; do
        if [[ $(date +%s) -ge $deadline ]]; then
            die "timed out after ${timeout}s waiting for $what"
        fi
        sleep 2
    done
}
